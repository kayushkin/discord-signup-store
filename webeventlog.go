package discordsignup

import (
	"fmt"
	"html/template"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// mapsURL opens a location in Google Maps: a Google Maps link pasted as the
// location — a dropped pin, shared — is opened as it is, and anything else
// is searched for.
func mapsURL(location string) string {
	location = strings.TrimSpace(location)
	if strings.HasPrefix(location, "https://") &&
		(strings.Contains(location, "google.") || strings.Contains(location, "goo.gl")) {
		return location
	}
	return "https://www.google.com/maps/search/?" + url.Values{"api": {"1"}, "query": {location}}.Encode()
}

// The event page's log: signups, edits and invites in one list, newest
// first. Three tables, because they are three kinds of fact, and one list,
// because an organiser asking "what happened" should not have to read three.
// Everything is rendered from what the rows hold at the moment the page is
// drawn; nothing here is stored.

// personHTML shows someone by their short name, with their Discord name
// behind it: on hover as a tooltip, and on a tap or click by opening it,
// since a phone has no hover. Someone with no short name is shown by their
// Discord name, and someone with neither — they left the server before
// anyone saw their name — by their id.
func personHTML(readableName, displayName, userID string) template.HTML {
	if readableName == "" {
		if displayName == "" {
			return template.HTML(template.HTMLEscapeString(userID))
		}
		return template.HTML(template.HTMLEscapeString(displayName))
	}
	if displayName == "" || displayName == readableName {
		return template.HTML(template.HTMLEscapeString(readableName))
	}
	// The caret says it opens; the box floats over the table rather than
	// widening its column.
	return template.HTML(fmt.Sprintf(
		`<details class="aka"><summary title="On Discord: %s">%s<svg class="caret" viewBox="0 0 10 10" aria-hidden="true"><path d="M2 3.5 5 6.5 8 3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg></summary>`+
			`<span class="aka-pop"><span class="aka-label">On Discord</span>%s</span></details>`,
		template.HTMLEscapeString(displayName), template.HTMLEscapeString(readableName),
		template.HTMLEscapeString(displayName)))
}

// localTimeHTML emits an instant for the page's script to rewrite into the
// reader's own zone. See the localTime template function.
func localTimeHTML(unix int64) template.HTML {
	if unix == 0 {
		return template.HTML("—")
	}
	t := time.Unix(unix, 0).UTC()
	return template.HTML(fmt.Sprintf(`<time class="ts" datetime="%s">%s</time>`,
		t.Format(time.RFC3339), t.Format("Mon 2 Jan 2006, 15:04")+" UTC"))
}

// dateBox is an event's date as a block — weekday, day, month — for the
// home page's cards. The page script rewrites it into the reader's zone; the
// server's text is the UTC fallback and says so.
func dateBox(unix int64) template.HTML {
	t := time.Unix(unix, 0).UTC()
	return template.HTML(fmt.Sprintf(`<time class="datebox" datetime="%s" title="%s UTC">`+
		`<span class="db-dow">%s</span><span class="db-day">%d</span><span class="db-mon">%s</span></time>`,
		t.Format(time.RFC3339), t.Format("Mon 2 Jan 2006, 15:04"), t.Format("Mon"), t.Day(), t.Format("Jan")))
}

// clockTime is the time of day alone, rewritten into the reader's zone.
func clockTime(unix int64) template.HTML {
	t := time.Unix(unix, 0).UTC()
	return template.HTML(fmt.Sprintf(`<time class="clock" datetime="%s">%s UTC</time>`,
		t.Format(time.RFC3339), t.Format("15:04")))
}

// eventLogEntry is one line of the log.
type eventLogEntry struct {
	At int64
	// Subject is the person it happened to, or empty for an edit to the event.
	Subject template.HTML
	What    template.HTML
	By      template.HTML
	// order breaks ties within one second: the three sources are merged, so
	// their ids cannot, and a signup and the promotion it caused share a
	// second more often than not.
	order int
}

// eventLogNames is what the log needs to put names on ids.
type eventLogNames struct {
	// actors maps an actor as recorded to who that is.
	actors map[string]actorName
	// people maps a Discord user id to a name, for the host field.
	people map[string]actorName
	// roles maps a role id to its name in the server.
	roles map[string]string
}

// actorName is a person the log names.
type actorName struct {
	ReadableName string
	DisplayName  string
	UserID       string
	// Via is the surface, "web" or "discord", when the actor recorded it.
	Via string
}

func (n eventLogNames) actor(actor string) template.HTML {
	a, ok := n.actors[actor]
	if !ok {
		return template.HTML(`<span class="muted">` + template.HTMLEscapeString(actor) + `</span>`)
	}
	out := personHTML(a.ReadableName, a.DisplayName, a.UserID)
	if a.Via != "" {
		out += template.HTML(` <span class="muted">(` + template.HTMLEscapeString(a.Via) + `)</span>`)
	}
	return out
}

// eventUpdateFieldWords are the edit log's field names as the page says them.
var eventUpdateFieldWords = map[string]string{
	"name":              "name",
	"description":       "description",
	"capacity":          "limit",
	"status":            "status",
	"starts_at":         "start",
	"ends_at":           "end",
	"location":          "place",
	"timezone":          "timezone",
	"recurrence_rule":   "repeats",
	"attending_role_id": "attending role",
	"waitlist_role_id":  "waitlist role",
	"waitlist_disabled": "waitlist",
	"created_by":        "host",
}

// eventUpdateValue renders one side of an edit. Values are stored raw, so
// this is where a time becomes a time and a role id becomes a role.
func (n eventLogNames) eventUpdateValue(field, value string) template.HTML {
	none := template.HTML(`<span class="muted">none</span>`)
	switch field {
	case "starts_at", "ends_at":
		unix, err := strconv.ParseInt(value, 10, 64)
		if err != nil || unix == 0 {
			return none
		}
		return localTimeHTML(unix)
	case "capacity":
		if value == "0" {
			return "no limit"
		}
	case "waitlist_disabled":
		if value == "true" {
			return "off"
		}
		return "on"
	case "recurrence_rule":
		return template.HTML(template.HTMLEscapeString(describeRepeat(value)))
	case "attending_role_id", "waitlist_role_id":
		if value == "" {
			return none
		}
		if name, ok := n.roles[value]; ok {
			return template.HTML("@" + template.HTMLEscapeString(name))
		}
	case "created_by":
		if p, ok := n.people[value]; ok {
			return personHTML(p.ReadableName, p.DisplayName, value)
		}
	}
	if value == "" {
		return none
	}
	return template.HTML(template.HTMLEscapeString(value))
}

// holdOutcomeWords say how a held place ended, in the log and the invite list.
var holdOutcomeWords = map[string]string{
	HoldOutcomeJoined:      "took the place held for them",
	HoldOutcomeDeclined:    "gave back the place held for them",
	HoldOutcomeReleased:    "held place released",
	HoldOutcomeUndelivered: "held place freed: the invite was not delivered",
	HoldOutcomeExpired:     "held place ended with the date",
}

// buildEventLog merges an event's three histories, newest first.
func buildEventLog(signups []SignupUpdate, edits []EventUpdate, invites []EventInvite, names eventLogNames) []eventLogEntry {
	out := make([]eventLogEntry, 0, len(signups)+len(edits)+len(invites))
	for i, u := range signups {
		what := u.Action
		switch {
		case u.Action == ActionMoved:
			what = "moved in the waitlist"
		case u.Action != u.ToState:
			what += " → " + u.ToState
		}
		out = append(out, eventLogEntry{At: u.At, order: i,
			Subject: personHTML(u.ReadableName, u.DisplayName, u.DiscordUserID),
			What:    template.HTML(template.HTMLEscapeString(what)),
			By:      names.actor(u.Actor)})
	}
	for i, u := range edits {
		label := eventUpdateFieldWords[u.Field]
		if label == "" {
			label = u.Field
		}
		var what template.HTML
		if u.Field == "description" {
			// A description can be paragraphs; the change is one line with
			// the new text behind it.
			what = template.HTML(`<details class="log-more"><summary>changed the description</summary><div class="muted">` +
				template.HTMLEscapeString(u.ToValue) + `</div></details>`)
		} else {
			what = template.HTML("changed the "+template.HTMLEscapeString(label)+": ") +
				names.eventUpdateValue(u.Field, u.FromValue) + " → " + names.eventUpdateValue(u.Field, u.ToValue)
		}
		out = append(out, eventLogEntry{At: u.At, order: len(signups) + i,
			Subject: template.HTML(`<span class="muted">the event</span>`),
			What:    what, By: names.actor(u.Actor)})
	}
	for i, inv := range invites {
		what := template.HTML("invited")
		switch inv.Delivery {
		case InviteDeliveryDMsClosed:
			what += ` <span class="bad">— not delivered, their DMs are closed</span>`
		case InviteDeliveryFailed:
			what += template.HTML(` <span class="bad">— not delivered: ` + template.HTMLEscapeString(inv.DeliveryError) + `</span>`)
		}
		if inv.HoldsPlace {
			what += " and held a place"
		}
		subject := personHTML(inv.ReadableName, inv.DisplayName, inv.DiscordUserID)
		out = append(out, eventLogEntry{At: inv.At, order: len(signups) + len(edits) + i,
			Subject: subject, What: what, By: names.actor(inv.InvitedBy)})
		if inv.HoldsPlace && inv.HoldEndedAt > 0 {
			by := names.actor(inv.HoldEndedBy)
			if inv.HoldEndedBy == "" {
				by = ""
			}
			out = append(out, eventLogEntry{At: inv.HoldEndedAt, order: len(signups) + len(edits) + len(invites) + i,
				Subject: subject, What: template.HTML(holdOutcomeWords[inv.HoldOutcome]), By: by})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		return out[i].order > out[j].order
	})
	return out
}
