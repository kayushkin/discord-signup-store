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

// shortTime is an instant as a short date and time — "Sep 25, 9:24 PM" —
// for lists, where the full form wraps on a phone. The full one is its
// tooltip; both are rewritten into the reader's zone.
func shortTime(unix int64) template.HTML {
	if unix == 0 {
		return template.HTML("—")
	}
	t := time.Unix(unix, 0).UTC()
	return template.HTML(fmt.Sprintf(`<time class="ts-short" datetime="%s">%s UTC</time>`,
		t.Format(time.RFC3339), t.Format("Jan 2, 15:04")))
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
	// avatars are the people with an approved avatar, shown beside their name.
	avatars map[string]bool
}

// person shows someone as personHTML does, after their avatar if they have one.
func (n eventLogNames) person(readableName, displayName, userID string) template.HTML {
	return avatarHTML(n.avatars, userID) + personHTML(readableName, displayName, userID)
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
	out := n.person(a.ReadableName, a.DisplayName, a.UserID)
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
	"pictures_disabled": "picture",
	"potluck_enabled":   "potluck sheet",
	"created_by":        "host",
}

// eventUpdateValue renders one side of an edit. Values are stored raw, so
// this is where a time becomes a time and a role id becomes a role.
func (n eventLogNames) eventUpdateValue(field, value string) template.HTML {
	none := template.HTML(`<span class="muted">none</span>`)
	if strings.HasPrefix(field, potluckWantedField) {
		if value == "0" {
			return template.HTML(`<span class="muted">no number</span>`)
		}
		return template.HTML(template.HTMLEscapeString(value))
	}
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
	case "waitlist_disabled", "pictures_disabled":
		if value == "true" {
			return "off"
		}
		return "on"
	case "potluck_enabled":
		if value == "true" {
			return "on"
		}
		return "off"
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
			return n.person(p.ReadableName, p.DisplayName, value)
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

// holdOutcomeText is how an invite's held place or pass ended.
func holdOutcomeText(inv EventInvite) string {
	if inv.PastLimit {
		switch inv.HoldOutcome {
		case HoldOutcomeJoined:
			return "joined past the limit"
		case HoldOutcomeDeclined:
			return "said they can't go"
		}
		return "pass past the limit ended (" + inv.HoldOutcome + ")"
	}
	return holdOutcomeWords[inv.HoldOutcome]
}

// buildEventLog merges an event's three histories, newest first.
// potluckLog is the potluck sheet's part of the event's log: every row the
// sheet has had, and every reminder it sent.
type potluckLog struct {
	rows      []PotluckHistoryRow
	reminders []PotluckReminder
	roles     []SheetRoleHistoryRow
	requests  []PotluckRequestHistoryRow
}


func buildEventLog(signups []SignupUpdate, edits []EventUpdate, invites []EventInvite, regulars []EventRegular, messages []EventMessage, potluck potluckLog, organisers []EventOrganiser, names eventLogNames) []eventLogEntry {
	out := make([]eventLogEntry, 0, len(signups)+len(edits)+len(invites)+len(regulars)+len(messages)+len(potluck.rows))
	out = append(out, potluckLogEntries(potluck, names)...)
	out = append(out, sheetRoleLogEntries(potluck.roles, names)...)
	out = append(out, potluckRequestLogEntries(potluck.requests, names)...)
	out = append(out, organiserLogEntries(organisers, names)...)
	for i, m := range messages {
		how := "in the forum thread"
		if m.Via == MessageViaDM {
			how = fmt.Sprintf("by DM, reaching %d of %d", m.Delivered, m.Recipients)
		}
		what := template.HTML(`<details class="log-more"><summary>messaged everyone ` +
			template.HTMLEscapeString(audienceWords(m.Audience)) + " " + template.HTMLEscapeString(how) +
			`</summary><div class="muted prose">` + template.HTMLEscapeString(m.Body) + `</div></details>`)
		if m.Status == messageFailed {
			what += template.HTML(` <span class="bad">— not sent: ` + template.HTMLEscapeString(m.Detail) + `</span>`)
		}
		out = append(out, eventLogEntry{At: m.At, order: 1_000_000 + i,
			Subject: template.HTML(`<span class="muted">` + strconv.Itoa(m.Recipients) + ` ` + plural(m.Recipients, "person", "people") + `</span>`),
			What:    what, By: names.actor(m.SentBy)})
	}
	base := len(signups) + len(edits) + 2*len(invites)
	for i, p := range regulars {
		subject := names.person(p.ReadableName, p.DisplayName, p.DiscordUserID)
		out = append(out, eventLogEntry{At: p.AddedAt, order: base + i, Subject: subject,
			What: "made a regular: on every date", By: names.actor(p.AddedBy)})
		if p.EndedAt > 0 {
			out = append(out, eventLogEntry{At: p.EndedAt, order: base + len(regulars) + i, Subject: subject,
				What: "no longer a regular", By: names.actor(p.EndedBy)})
		}
	}
	for i, u := range signups {
		what := u.Action
		switch {
		case u.Action == ActionMoved:
			what = "moved in the waitlist"
		case u.Action != u.ToState:
			what += " → " + u.ToState
		}
		out = append(out, eventLogEntry{At: u.At, order: i,
			Subject: names.person(u.ReadableName, u.DisplayName, u.DiscordUserID),
			What:    template.HTML(template.HTMLEscapeString(what)),
			By:      names.actor(u.Actor)})
	}
	for i, u := range edits {
		label := eventUpdateFieldWords[u.Field]
		if c, ok := potluckCategoryOf(strings.TrimPrefix(u.Field, potluckWantedField)); ok && strings.HasPrefix(u.Field, potluckWantedField) {
			label = "potluck's " + strings.ToLower(c.Label) + " wanted"
		}
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
		if inv.PastLimit {
			what += ", able to join past the limit"
		}
		subject := names.person(inv.ReadableName, inv.DisplayName, inv.DiscordUserID)
		out = append(out, eventLogEntry{At: inv.At, order: len(signups) + len(edits) + i,
			Subject: subject, What: what, By: names.actor(inv.InvitedBy)})
		if (inv.HoldsPlace || inv.PastLimit) && inv.HoldEndedAt > 0 {
			by := names.actor(inv.HoldEndedBy)
			if inv.HoldEndedBy == "" {
				by = ""
			}
			out = append(out, eventLogEntry{At: inv.HoldEndedAt, order: len(signups) + len(edits) + len(invites) + i,
				Subject: subject, What: template.HTML(holdOutcomeText(inv)), By: by})
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

// potluckLogEntries are the potluck sheet's lines in the event's log: each
// thing someone said they would bring, each taken off and by whom, and each
// round of reminder DMs a day before. A "not bringing anything" ended by the
// same person adding something in the same moment is not a line of its own:
// the add says it.
func potluckLogEntries(potluck potluckLog, names eventLogNames) []eventLogEntry {
	addedAt := map[string]map[int64]bool{}
	for _, h := range potluck.rows {
		if addedAt[h.DiscordUserID] == nil {
			addedAt[h.DiscordUserID] = map[int64]bool{}
		}
		addedAt[h.DiscordUserID][h.AddedAt] = true
	}
	thing := func(h PotluckHistoryRow) template.HTML {
		if h.Category == potluckNothing {
			return "not bringing anything"
		}
		c, _ := potluckCategoryOf(h.Category)
		return template.HTML(template.HTMLEscapeString(c.Emoji + " " + h.Item))
	}
	muted := func(text string) template.HTML {
		return template.HTML(`<span class="muted">` + template.HTMLEscapeString(text) + `</span>`)
	}
	var out []eventLogEntry
	for i, h := range potluck.rows {
		subject := names.person(h.ReadableName, h.DisplayName, h.DiscordUserID)
		by := subject
		if h.Source == potluckSourceSheet {
			by = muted("copied from the spreadsheet")
		}
		what := "potluck: bringing " + thing(h)
		if h.Category == potluckNothing {
			what = "potluck: " + thing(h)
		}
		out = append(out, eventLogEntry{At: h.AddedAt, order: 2_000_000 + i, Subject: subject, What: what, By: by})
		if h.RemovedAt == 0 {
			continue
		}
		if h.Category == potluckNothing && (h.RemovedBy == ActorUser || h.RemovedBy == actorSheetImport) &&
			addedAt[h.DiscordUserID][h.RemovedAt] {
			continue
		}
		switch h.RemovedBy {
		case ActorUser:
			by = subject
		case actorSheetImport:
			by = muted("their row in the spreadsheet changed")
		case ActorRecurrence:
			by = muted("the next date")
		default:
			by = names.actor(h.RemovedBy)
		}
		what = "potluck: took off " + thing(h)
		if h.Category == potluckNothing {
			what = "potluck: no longer “not bringing anything”"
		}
		out = append(out, eventLogEntry{At: h.RemovedAt, order: 3_000_000 + i, Subject: subject, What: what, By: by})
	}
	// The reminder a day before goes to everyone due in one run; the run is
	// one line. Nudges are an organiser's messages and are logged as those.
	for i := 0; i < len(potluck.reminders); {
		r := potluck.reminders[i]
		if r.Kind != PotluckReminderAutomatic {
			i++
			continue
		}
		sent, total := 0, 0
		j := i
		for ; j < len(potluck.reminders) && potluck.reminders[j].Kind == PotluckReminderAutomatic &&
			potluck.reminders[j].SentAt-r.SentAt < 60; j++ {
			total++
			if potluck.reminders[j].Outcome == potluckReminderSent {
				sent++
			}
		}
		out = append(out, eventLogEntry{At: r.SentAt, order: 4_000_000 + i,
			Subject: muted(strconv.Itoa(total) + " " + plural(total, "person", "people")),
			What: template.HTML(template.HTMLEscapeString(fmt.Sprintf(
				"potluck: reminded by DM a day before, reaching %d of %d", sent, total))),
			By: muted("automatic")})
		i = j
	}
	return out
}
