package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"strings"
	"sync"
	"time"
)

const (
	syncInterval       = time.Minute
	syncForceFrequency = 24 * 60
)

type Config struct {
	Homeserver  string        `json:"homeserver"`
	UserID      id.UserID     `json:"user_id"`
	Password    string        `json:"password,omitempty"`
	DeviceID    id.DeviceID   `json:"device_id,omitempty"`
	AccessToken string        `json:"access_token,omitempty"`
	Rooms       [][]id.RoomID `json:"rooms"`
}

func (c *Config) Load() error {
	log.Printf("Loading config.")
	data, err := ioutil.ReadFile("config.json")
	if err != nil {
		return err
	}
	return json.Unmarshal(data, c)
}

func (c *Config) Save() error {
	log.Printf("Saving config.")
	data, err := json.MarshalIndent(c, "", "\t")
	if err != nil {
		return err
	}
	return ioutil.WriteFile("config.json", data, 0700)
}

func Login(config *Config) (*mautrix.Client, error) {
	// Note: we have to lower case the user ID for Matrix protocol communication.
	uid := id.UserID(strings.ToLower(string(config.UserID)))
	client, err := mautrix.NewClient(config.Homeserver, uid, config.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}
	if config.AccessToken == "" {
		resp, err := client.Login(&mautrix.ReqLogin{
			Type: mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{
				Type: mautrix.IdentifierTypeUser,
				User: string(client.UserID),
			},
			Password:                 config.Password,
			InitialDeviceDisplayName: "matrixbot",
			StoreCredentials:         true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to authenticate: %v", err)
		}
		config.Password = ""
		config.DeviceID = resp.DeviceID
		config.AccessToken = resp.AccessToken
		err = config.Save()
		if err != nil {
			return nil, fmt.Errorf("failed to save config: %v", err)
		}
	} else {
		client.DeviceID = config.DeviceID
	}
	return client, nil
}

var (
	roomUsers       = map[id.RoomID]map[id.UserID]struct{}{}
	roomUsersMu     sync.RWMutex
	fullySynced     bool
	roomPowerLevels = map[id.RoomID]*event.PowerLevelsEventContent{}
)

func setUserStateAt(room id.RoomID, user id.UserID, now time.Time, maxPrevState, state State) {
	err := writeUserStateAt(room, user, now, maxPrevState, state)
	if err != nil {
		log.Fatalf("failed to write user state: %v", err)
	}
}

func handleMessage(now time.Time, room id.RoomID, sender id.UserID, raw *event.Event) {
	// log.Printf("[%v] Message from %v to %v", now, sender, room)
	roomUsersMu.Lock()
	roomUsers[room][sender] = struct{}{}
	roomUsersMu.Unlock()
	setUserStateAt(room, sender, now.Add(-activeTime), Active, Active)
	setUserStateAt(room, sender, now, Active, Idle)
}

func handleJoin(now time.Time, room id.RoomID, member id.UserID, raw *event.Event) {
	log.Printf("[%v] Join from %v to %v", now, member, room)
	roomUsersMu.Lock()
	roomUsers[room][member] = struct{}{}
	roomUsersMu.Unlock()
	setUserStateAt(room, member, now, NotActive, Idle)
}

func handleLeave(now time.Time, room id.RoomID, member id.UserID, raw *event.Event) {
	log.Printf("[%v] Leave from %v to %v", now, member, room)
	roomUsersMu.Lock()
	delete(roomUsers[room], member)
	roomUsersMu.Unlock()
	setUserStateAt(room, member, now, Active, NotActive)
}

func handlePowerLevels(now time.Time, room id.RoomID, levels *event.PowerLevelsEventContent, raw *event.Event) {
	// log.Printf("[%v] Power levels for %v are %v", now, room, levels)
	levelsCopy := *levels // Looks like mautrix always passes the same pointer here.
	roomUsersMu.Lock()
	roomPowerLevels[room] = &levelsCopy
	roomUsersMu.Unlock()
}

func eventTime(evt *event.Event) time.Time {
	return time.Unix(0, evt.Timestamp*1000000)
}

type MoreMessagesSyncer struct {
	*mautrix.DefaultSyncer
}

func newSyncer() *MoreMessagesSyncer {
	return &MoreMessagesSyncer{
		DefaultSyncer: mautrix.NewDefaultSyncer(),
	}
}

func (s *MoreMessagesSyncer) GetFilterJSON(userID id.UserID) *mautrix.Filter {
	f := s.DefaultSyncer.GetFilterJSON(userID)
	// Same filters as Element.
	f.Room.Timeline.Limit = 20
	// Only include our rooms.
	f.Room.Rooms = make([]id.RoomID, 0, len(roomUsers))
	for room := range roomUsers {
		f.Room.Rooms = append(f.Room.Rooms, room)
	}
	return f
}

func isRoom(room id.RoomID) bool {
	roomUsersMu.RLock()
	defer roomUsersMu.RUnlock()
	_, found := roomUsers[room]
	return found
}

func Run() (err error) {
	err = InitDatabase()
	if err != nil {
		return fmt.Errorf("failed to init database: %v", err)
	}
	defer func() {
		err2 := CloseDatabase()
		if err2 != nil && err == nil {
			err = fmt.Errorf("failed to close database: %v", err)
		}
	}()
	logPowerLevelBounds()
	config := &Config{}
	err = config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %v", err)
	}
	for _, group := range config.Rooms {
		for _, room := range group {
			roomUsers[room] = map[id.UserID]struct{}{}
		}
	}
	client, err := Login(config)
	if err != nil {
		return fmt.Errorf("failed to login: %v", err)
	}
	syncer := newSyncer()
	syncer.OnEventType(event.EventMessage, func(source mautrix.EventSource, evt *event.Event) {
		if !isRoom(evt.RoomID) {
			return
		}
		handleMessage(eventTime(evt), evt.RoomID, evt.Sender, evt)
	})
	syncer.OnEventType(event.StateMember, func(source mautrix.EventSource, evt *event.Event) {
		if !isRoom(evt.RoomID) {
			return
		}
		mem := evt.Content.AsMember()
		switch mem.Membership {
		case event.MembershipJoin:
			handleJoin(eventTime(evt), evt.RoomID, evt.Sender, evt)
		case event.MembershipLeave:
			handleLeave(eventTime(evt), evt.RoomID, evt.Sender, evt)
		default: // Ignore.
		}
	})
	syncer.OnEventType(event.StatePowerLevels, func(source mautrix.EventSource, evt *event.Event) {
		if !isRoom(evt.RoomID) {
			return
		}
		handlePowerLevels(eventTime(evt), evt.RoomID, evt.Content.AsPowerLevels(), evt)
	})
	syncer.OnSync(func(resp *mautrix.RespSync, since string) bool {
		// j, _ := json.MarshalIndent(resp, "", "  ")
		// log.Print(string(j))
		roomUsersMu.Lock()
		if since != "" && !fullySynced {
			log.Print("Fully synced.")
			for room, users := range roomUsers {
				if len(users) == 0 {
					log.Printf("Not actually joined %v yet...", room)
					_, err := client.JoinRoom(string(room), "", nil)
					if err != nil {
						log.Printf("Failed to join %v: %v", room, err)
					}
				}
			}
			fullySynced = true
		}
		roomUsersMu.Unlock()
		return true
	})
	client.Syncer = syncer
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	go func() {
		counter := 0
		for range ticker.C {
			roomUsersMu.RLock()
			scoreData := map[id.RoomID]map[id.UserID]*Score{}
			now := time.Now()
			for room := range roomUsers {
				scores, err := queryUserScores(room, now)
				if err != nil {
					log.Fatalf("failed to query user scores: %v", err)
				}
				scoreData[room] = scores
			}
			for _, group := range config.Rooms {
				for _, room := range group {
					syncPowerLevels(client, room, group, scoreData, counter%syncForceFrequency == 0)
				}
			}
			roomUsersMu.RUnlock()
			counter++
		}
	}()
	return client.Sync()
}

func main() {
	err := Run()
	if err != nil {
		log.Fatalf("Program failed: %v", err)
	}
}