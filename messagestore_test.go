package discordsignup

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestADMChannelIsFiledWithMessageStoreOnceWithTheBotAndThePerson(t *testing.T) {
	var mu sync.Mutex
	var batches []map[string]any
	var paths []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/imports":
			w.Write([]byte(`{"id":"message_import_000001"}`))
		case "/imports/message_import_000001/batches":
			var batch map[string]any
			json.Unmarshal(body, &batch)
			batches = append(batches, batch)
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer fake.Close()
	client := NewMessageStoreClient(fake.URL)
	channels := []DirectMessageChannel{{ChannelID: "300", DiscordUserID: "42"}}
	if err := client.FileDirectMessageChannels("900", channels); err != nil {
		t.Fatal(err)
	}
	if err := client.FileDirectMessageChannels("900", channels); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("filed %d times, want once: %v", len(batches), paths)
	}
	conversation := batches[0]["conversations"].([]any)[0].(map[string]any)
	participants := conversation["participants"].([]any)
	if conversation["source_ref"] != "300" || conversation["is_group"] != false || len(participants) != 2 ||
		participants[0].(map[string]any)["handle"] != "900" || participants[1].(map[string]any)["handle"] != "42" {
		t.Fatalf("conversation = %v", conversation)
	}
	if NewMessageStoreClient("") != nil {
		t.Fatal("no URL should mean no client")
	}
	if err := client.FileDirectMessageChannels("", []DirectMessageChannel{{ChannelID: "301"}}); err == nil {
		t.Fatal("an unknown bot id must be an error, not a conversation without the bot")
	}
}
