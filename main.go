package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/policies"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
	decodepay "github.com/nbd-wtf/ln-decodepay"
	"log"
	"net/http"
	"regexp"
	"time"
)

type Description struct {
	PubKey    string     `json:"pubkey"`
	Content   string     `json:"content"`
	ID        string     `json:"id"`
	CreatedAt int64      `json:"created_at"`
	Sig       string     `json:"sig"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
}

var (
	relays = []string{
		"wss://relay.snort.social",
		"wss://nos.lol",
		"wss://nostr.mom",
		"wss://nostr.wine",
		"wss://relay.damus.io",
		"wss://relay.nostr.band",
		"wss://purplepag.es",
		"wss://relay.nostr.land",
		"wss://relay.primal.net",
	}
	botPubkey string
	relay     = khatru.NewRelay()
	pool      = nostr.NewSimplePool(context.Background())
	port      = 3456
)

func main() {
	relay.Info.Name = "PPE Relay"
	relay.Info.PubKey = "f1f9b0996d4ff1bf75e79e4cc8577c89eb633e68415c7faf74cf17a07bf80bd8"
	relay.Info.Description = "Pay-Per-Event Relay."

	godotenv.Load(".env")
	botPubkey, _ = nostr.GetPublicKey(GetEnv("BOT_PRIVATE_KEY"))

	db := sqlite3.SQLite3Backend{DatabaseURL: "./db/db"}
	if err := db.Init(); err != nil {
		panic(err)
	}

	relay.RejectEvent = append(relay.RejectEvent,
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(5, time.Minute*1, 30),
		policies.RestrictToSpecifiedKinds([]uint16{1, 30023}...),
	)

	relay.RejectFilter = append(relay.RejectFilter,
		policies.NoEmptyFilters,
		policies.NoComplexFilters,
	)

	relay.RejectConnection = append(relay.RejectConnection,
		policies.ConnectionRateLimiter(10, time.Minute*2, 30),
	)

	relay.RejectEvent = append(relay.RejectEvent, func(ctx context.Context, event *nostr.Event) (reject bool, msg string) {
		userPaidAmount := GetZapsTotalFromUser(event.PubKey)
		userNotesCount := GetStoredEventsCountFromUser(event.PubKey, db)

		if userPaidAmount < (userNotesCount + 1) {
			return true, "no sufficient balance; top up"
		}
		return false, ""
	})

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)

	fmt.Printf("Running on :%v", port)

	go HandleBotCommands(db)

	http.ListenAndServe(fmt.Sprintf(":%v", port), relay)
}

func GetZapEventsFromUser(pubkey string) map[string]*nostr.Event {
	ctx := context.Background()

	events := make(map[string]*nostr.Event)

	tags := make(nostr.TagMap)
	tags["p"] = []string{botPubkey}
	filter := nostr.Filter{
		Kinds: []int{nostr.KindZap},
		Tags:  tags,
	}

	for event := range pool.SubManyEose(ctx, relays, []nostr.Filter{filter}) {
		zapRequest, err := GetZapRequestFromZapEvent(event.Event)
		if err != nil {
			continue
		} else if zapRequest.PubKey == pubkey {
			events[event.ID] = event.Event
		}
	}
	return events
}

func GetZapRequestFromZapEvent(event *nostr.Event) (*Description, error) {
	var descriptionJSON string
	for _, tag := range event.Tags {
		if len(tag) > 1 && tag[0] == "description" {
			descriptionJSON = tag[1]
			break
		}
	}

	if descriptionJSON == "" {
		return nil, errors.New("description tag not found")
	}

	var description Description
	err := json.Unmarshal([]byte(descriptionJSON), &description)
	if err != nil {
		fmt.Printf("Error parsing description: %v\n", err)
		return nil, fmt.Errorf("error parsing description: %v", err)
	}
	return &description, nil
}

func GetZapsTotalFromUser(pubkey string) int64 {
	zapEvents := GetZapEventsFromUser(pubkey)

	total := int64(0)

	for _, event := range zapEvents {
		bolt11, err := ValueFromTag(event, "bolt11")
		if err != nil {
			continue
		} else if bolt11 != nil {
			decoded, err := decodepay.Decodepay(*bolt11)
			if err != nil {
				continue
			} else {
				total += decoded.MSatoshi
			}
		}
	}
	return total / 1000
}

func GetStoredEventsCountFromUser(pubkey string, db sqlite3.SQLite3Backend) int64 {
	ctx := context.Background()

	filter := nostr.Filter{
		Authors: []string{pubkey},
	}

	iCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	count, err := db.CountEvents(iCtx, filter)
	if err != nil {
		log.Fatalf("Failed to query events: %v", err)
	}
	return count
}

func GetRemainingUserBalance(pubkey string, db sqlite3.SQLite3Backend) int64 {
	userPaidAmount := GetZapsTotalFromUser(pubkey)
	userNotesCount := GetStoredEventsCountFromUser(pubkey, db)

	remainingBalance := userPaidAmount - userNotesCount
	return remainingBalance
}

func HandleBotCommands(db sqlite3.SQLite3Backend) {
	ctx := context.Background()

	tags := make(nostr.TagMap)
	tags["p"] = []string{botPubkey}
	filter := nostr.Filter{
		Kinds: []int{nostr.KindTextNote},
		Tags:  tags,
	}

	for event := range pool.SubMany(ctx, relays, []nostr.Filter{filter}) {
		if !BotCommandFulfilled(event.ID) {
			balanceRequest, _ := regexp.MatchString(`(?mi)\bbalance\b`, event.Content)
			if balanceRequest {
				userBalance := GetRemainingUserBalance(event.PubKey, db)
				response := fmt.Sprintf("Your balance is %v sats.", userBalance)

				PublishCommandResponseEvent(event.Event, response)
			}
		}
	}
}

func BotCommandFulfilled(ID string) bool {
	ctx := context.Background()

	tags := make(nostr.TagMap)
	tags["e"] = []string{ID}
	filter := nostr.Filter{
		Kinds:   []int{nostr.KindTextNote},
		Tags:    tags,
		Authors: []string{botPubkey},
	}

	for range pool.SubManyEose(ctx, relays, []nostr.Filter{filter}) {
		return true
	}
	return false
}

func PublishCommandResponseEvent(ev *nostr.Event, content string) {
	event := nostr.Event{
		PubKey:    botPubkey,
		CreatedAt: nostr.Now(),
		Kind:      nostr.KindTextNote,
		Content:   content,
		Tags:      []nostr.Tag{[]string{"e", ev.ID}, []string{"p", ev.PubKey}},
	}
	event.Sign(GetEnv("GM_BOT_PRIVATE_KEY"))

	ctx := context.Background()

	for _, url := range relays {
		relay, err := nostr.RelayConnect(ctx, url)
		if err != nil {
			fmt.Println(err)
			continue
		}
		if err := relay.Publish(ctx, event); err != nil {
			fmt.Println(err)
			continue
		}

		fmt.Printf("published to %s\n", url)
	}
}
