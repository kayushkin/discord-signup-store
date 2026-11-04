package discordsignup

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A role's emoji is one of two kinds, stored as Discord writes it in a
// message so one string serves every surface that renders Discord markdown:
//
//   - a standard emoji, from standardEmojiGroups (emoji_data_generated.go,
//     made by art/generate-emoji-data.mjs from the same package bridge-ui's
//     chat picker uses), stored as the emoji itself;
//   - one of the event's server's own custom emojis, stored as <:name:id>,
//     or <a:name:id> when it moves. Discord lets a bot use a custom emoji
//     from any server it is in, so the server's own are always usable.
//
// The picker offers the server's emojis first, then the standard ones.

// standardEmojiGroup is one group of the picker, such as "Food & Drink".
type standardEmojiGroup struct {
	Name   string
	Emojis []standardEmoji
}

// standardEmoji is one standard emoji and its Unicode name, which the
// picker's search matches.
type standardEmoji struct {
	Emoji string
	Name  string
}

var (
	standardEmojiSetOnce sync.Once
	standardEmojiSet     map[string]bool
)

// isStandardEmoji says whether an emoji is one of standardEmojiGroups.
func isStandardEmoji(emoji string) bool {
	standardEmojiSetOnce.Do(func() {
		standardEmojiSet = map[string]bool{}
		for _, g := range standardEmojiGroups {
			for _, e := range g.Emojis {
				standardEmojiSet[e.Emoji] = true
			}
		}
	})
	return standardEmojiSet[emoji]
}

// customEmojiPattern is a custom emoji as a message writes it.
var customEmojiPattern = regexp.MustCompile(`^<(a?):([A-Za-z0-9_]{2,32}):([0-9]{1,20})>$`)

// customEmoji is a custom emoji's parts, read from <:name:id>.
type customEmoji struct {
	Name, ID string
	Animated bool
}

// parseCustomEmoji reads <:name:id> or <a:name:id>.
func parseCustomEmoji(emoji string) (customEmoji, bool) {
	m := customEmojiPattern.FindStringSubmatch(emoji)
	if m == nil {
		return customEmoji{}, false
	}
	return customEmoji{Name: m[2], ID: m[3], Animated: m[1] == "a"}, true
}

// customEmojiText writes a custom emoji as a message does.
func customEmojiText(e customEmoji) string {
	prefix := ""
	if e.Animated {
		prefix = "a"
	}
	return "<" + prefix + ":" + e.Name + ":" + e.ID + ">"
}

// isRoleEmojiForm says whether a stored emoji is either kind. Whether a
// custom one belongs to the event's server is checked where the server's
// list is known: Server.checkRoleEmoji.
func isRoleEmojiForm(emoji string) bool {
	if isStandardEmoji(emoji) {
		return true
	}
	_, ok := parseCustomEmoji(emoji)
	return ok
}

// roleEmojiPlainText is a role's emoji where Discord markdown is not
// rendered — the native event's description: a custom one as :name:.
func roleEmojiPlainText(emoji string) string {
	if c, ok := parseCustomEmoji(emoji); ok {
		return ":" + c.Name + ":"
	}
	return emoji
}

// roleEmojiButton is a role's emoji as a button's emoji object.
func roleEmojiButton(emoji string) map[string]any {
	if c, ok := parseCustomEmoji(emoji); ok {
		return map[string]any{"id": c.ID, "name": c.Name, "animated": c.Animated}
	}
	return map[string]any{"name": emoji}
}

// customEmojiImageURL is where Discord serves a custom emoji's picture.
func customEmojiImageURL(c customEmoji) string {
	url := "https://cdn.discordapp.com/emojis/" + c.ID + ".webp?size=48"
	if c.Animated {
		url += "&animated=true"
	}
	return url
}

// roleEmojiHTML is a role's emoji on a web page: a standard one as text, a
// custom one as Discord's picture of it.
func roleEmojiHTML(emoji string) template.HTML {
	if c, ok := parseCustomEmoji(emoji); ok {
		return template.HTML(fmt.Sprintf(`<img class="custom-emoji" src="%s" alt=":%s:" title=":%s:" width="22" height="22">`,
			template.HTMLEscapeString(customEmojiImageURL(c)), c.Name, c.Name))
	}
	return template.HTML(template.HTMLEscapeString(emoji))
}

// GuildEmoji is one of a server's custom emojis, as Discord lists it.
type GuildEmoji struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Animated  bool   `json:"animated"`
	Available bool   `json:"available"`
}

// ListGuildEmojis reads a server's custom emojis. Any member may, so the
// bot needs no permission for it.
func (c *DiscordClient) ListGuildEmojis(guildID string) ([]GuildEmoji, error) {
	raw, err := c.do(http.MethodGet, "/guilds/"+escapePathSegment(guildID)+"/emojis", nil)
	if err != nil {
		return nil, err
	}
	var out []GuildEmoji
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode guild emojis: %w", err)
	}
	return out, nil
}

// guildEmojiCacheFor is how long a server's emoji list is kept before it is
// read again. A page with five roles opens the picker five times; a new
// emoji added in Discord shows within this long.
const guildEmojiCacheFor = 5 * time.Minute

type guildEmojiCacheEntry struct {
	at     time.Time
	emojis []GuildEmoji
}

// guildEmojiCache keeps each server's emoji list for guildEmojiCacheFor. A
// failed read is not kept, so the next page asks again.
type guildEmojiCache struct {
	mu      sync.Mutex
	entries map[string]guildEmojiCacheEntry
}

// guildEmojis is a server's usable custom emojis, from the cache or
// Discord. An emoji Discord marks unavailable — the server lost the boost
// that paid for it — is left out, since a message cannot show it.
func (s *Server) guildEmojis(guildID string) ([]GuildEmoji, error) {
	if s.discord == nil {
		return nil, fmt.Errorf("no discord client configured")
	}
	s.emojiCache.mu.Lock()
	entry, ok := s.emojiCache.entries[guildID]
	s.emojiCache.mu.Unlock()
	if ok && time.Since(entry.at) < guildEmojiCacheFor {
		return entry.emojis, nil
	}
	all, err := s.discord.ListGuildEmojis(guildID)
	if err != nil {
		return nil, fmt.Errorf("list the emojis of guild %s: %w", guildID, err)
	}
	usable := make([]GuildEmoji, 0, len(all))
	for _, e := range all {
		if e.Available {
			usable = append(usable, e)
		}
	}
	s.emojiCache.mu.Lock()
	if s.emojiCache.entries == nil {
		s.emojiCache.entries = map[string]guildEmojiCacheEntry{}
	}
	s.emojiCache.entries[guildID] = guildEmojiCacheEntry{at: time.Now(), emojis: usable}
	s.emojiCache.mu.Unlock()
	return usable, nil
}

// checkRoleEmoji says whether an organizer may give a role this emoji on an
// event in guildID, and returns it as it should be stored: a standard emoji
// as it is, and a custom one rebuilt from Discord's own record of it, found
// by id, so a name typed into the form is never trusted.
func (s *Server) checkRoleEmoji(guildID, emoji string) (string, error) {
	if isStandardEmoji(emoji) {
		return emoji, nil
	}
	picked, ok := parseCustomEmoji(emoji)
	if !ok {
		return "", fmt.Errorf("%w: pick one of the emojis offered", ErrInvalidSheetRole)
	}
	emojis, err := s.guildEmojis(guildID)
	if err != nil {
		return "", err
	}
	for _, e := range emojis {
		if e.ID == picked.ID {
			return customEmojiText(customEmoji{Name: e.Name, ID: e.ID, Animated: e.Animated}), nil
		}
	}
	return "", fmt.Errorf("%w: that emoji is not one of this server's", ErrInvalidSheetRole)
}

// roleEmojiPicker is what the page's one emoji picker offers: the server's
// own emojis, then the standard ones. ServerError says why the server's
// could not be read, shown in the picker rather than hidden.
type roleEmojiPicker struct {
	Server      []roleEmojiChoice
	ServerError string
	Standard    []roleEmojiGroupChoices
}

type roleEmojiGroupChoices struct {
	Name   string
	Emojis []roleEmojiChoice
}

// roleEmojiChoice is one button in the picker: the value the form sends,
// what the button shows, and the words the search matches.
type roleEmojiChoice struct {
	Value  string
	Shown  template.HTML
	Search string
}

// roleEmojiPickerFor builds the picker for an event's server.
func (s *Server) roleEmojiPickerFor(guildID string) *roleEmojiPicker {
	picker := &roleEmojiPicker{}
	if emojis, err := s.guildEmojis(guildID); err != nil {
		picker.ServerError = err.Error()
	} else {
		for _, e := range emojis {
			value := customEmojiText(customEmoji{Name: e.Name, ID: e.ID, Animated: e.Animated})
			picker.Server = append(picker.Server, roleEmojiChoice{Value: value, Shown: roleEmojiHTML(value),
				Search: strings.ToLower(strings.ReplaceAll(e.Name, "_", " "))})
		}
	}
	for _, g := range standardEmojiGroups {
		group := roleEmojiGroupChoices{Name: g.Name}
		for _, e := range g.Emojis {
			group.Emojis = append(group.Emojis, roleEmojiChoice{Value: e.Emoji,
				Shown: template.HTML(template.HTMLEscapeString(e.Emoji)), Search: e.Name})
		}
		picker.Standard = append(picker.Standard, group)
	}
	return picker
}
