// Command discord-event-picture-painter paints the picture of who is going to
// each event on the events site's home page. It asks discord-signup-store
// which events' pictures no longer show who is going, paints each from the
// people's avatar drawings with art/render-event-picture.mjs, and hands the
// print back. No model is involved. The scheduler runs it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type picture struct {
	EventID   int64             `json:"event_id"`
	Signature string            `json:"signature"`
	People    []json.RawMessage `json:"people"`
}

func main() {
	storeURL := flag.String("store-url", "", "discord-signup-store's address, such as http://127.0.0.1:8312 (required)")
	artDirectory := flag.String("art-directory", "", "the repo's art directory, holding kit.js, render-event-picture.mjs and node_modules (required)")
	chromePath := flag.String("chrome", "", "the Chrome or Chromium that paints (required)")
	nodePath := flag.String("node", "node", "the node that runs render-event-picture.mjs")
	flag.Parse()
	for name, value := range map[string]string{"-store-url": *storeURL, "-art-directory": *artDirectory, "-chrome": *chromePath} {
		if value == "" {
			log.Fatalf("%s is required", name)
		}
	}
	base := strings.TrimRight(*storeURL, "/")
	client := &http.Client{Timeout: time.Minute}

	var due struct {
		Pictures []picture `json:"pictures"`
	}
	if err := call(client, http.MethodGet, base+"/api/event-pictures/due", nil, &due); err != nil {
		log.Fatalf("list pictures due: %v", err)
	}
	failed := 0
	for _, p := range due.Pictures {
		image, err := paint(p, *artDirectory, *chromePath, *nodePath)
		if err == nil {
			err = call(client, http.MethodPut, fmt.Sprintf("%s/api/events/%d/picture", base, p.EventID),
				map[string]any{"signature": p.Signature, "image_webp": image}, nil)
		}
		if err != nil {
			// A roster that changed while this painted is a 409, and the
			// next run paints it again; anything else is a failure to see.
			log.Printf("event %d: %v", p.EventID, err)
			failed++
			continue
		}
		log.Printf("painted event %d with %d people", p.EventID, len(p.People))
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func paint(p picture, artDirectory, chromePath, nodePath string) ([]byte, error) {
	folder, err := os.MkdirTemp("", "discord-event-picture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(folder)
	people, err := json.Marshal(p.People)
	if err != nil {
		return nil, err
	}
	peopleFile, outFile := filepath.Join(folder, "people.json"), filepath.Join(folder, "picture.webp")
	if err := os.WriteFile(peopleFile, people, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, nodePath, filepath.Join(artDirectory, "render-event-picture.mjs"), peopleFile, outFile)
	command.Dir = artDirectory
	command.Env = append(os.Environ(), "CHROME_PATH="+chromePath)
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("paint: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return os.ReadFile(outFile)
}

// call sends a JSON request and decodes a JSON answer into out.
func call(client *http.Client, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s answered %d: %s", method, url, response.StatusCode, strings.TrimSpace(string(answer)))
	}
	if out != nil {
		return json.Unmarshal(answer, out)
	}
	return nil
}
