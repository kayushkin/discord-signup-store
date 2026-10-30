package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// The potluck sheet on Discord: the What I'm bringing button on the card
// and in the reminder DMs opens a private view of the sheet with a button
// per category; a category button opens a one-line form; and Not bringing
// anything and Clear mine answer at once. Every press after the first edits
// that private view in place, so it always shows the sheet as it now is.

const (
	potluckAction        = "potluck"
	potluckAddPrefix     = "potluck-add-"
	potluckModalPrefix   = "potluck-modal-"
	potluckNothingAction = "potluck-nothing"
	potluckClearAction   = "potluck-clear"
	// fieldPotluckItem is the one input on the potluck form.
	fieldPotluckItem = "potluck_item"
)

// PotluckCustomID is the What I'm bringing button.
func PotluckCustomID(eventID int64) string {
	return fmt.Sprintf("%s:%s:%d", customIDPrefix, potluckAction, eventID)
}

func potluckAddCustomID(eventID int64, category string) string {
	return fmt.Sprintf("%s:%s%s:%d", customIDPrefix, potluckAddPrefix, category, eventID)
}

func potluckModalCustomID(eventID int64, category string) string {
	return fmt.Sprintf("%s:%s%s:%d", customIDPrefix, potluckModalPrefix, category, eventID)
}

func isPotluckAction(action string) bool {
	return action == potluckAction || strings.HasPrefix(action, "potluck-")
}

// handlePotluckComponent answers a press of any potluck button.
func (s *Server) handlePotluckComponent(w http.ResponseWriter, in *Interaction, action string, eventID int64, userID string) {
	switch {
	case action == potluckAction:
		s.replyPotluckPanel(w, callbackTypeChannelMessageWithSrc, eventID, userID, "")
	case strings.HasPrefix(action, potluckAddPrefix):
		category, ok := potluckCategoryOf(strings.TrimPrefix(action, potluckAddPrefix))
		if !ok {
			s.replyEphemeral(w, "Unknown potluck category.")
			return
		}
		// The form's example is what the organiser asked for in this
		// category, while any of it is still open.
		example := category.Example
		if ev, err := s.store.GetEvent(eventID); err != nil {
			log.Printf("[discord-signup] potluck form event=%d: %v", eventID, err)
		} else if ev.Potluck != nil {
			if asked := ev.Potluck.askedForIn(category.Key); asked != "" {
				example = truncate(asked, 100)
			}
		}
		writeJSON(w, 200, map[string]any{
			"type": callbackTypeModal,
			"data": map[string]any{
				"custom_id": potluckModalCustomID(eventID, category.Key),
				"title":     truncate(category.Emoji+" "+category.Label, 45),
				"components": []any{map[string]any{"type": componentTypeActionRow, "components": []any{
					modalTextInput(fieldPotluckItem+"@"+potluckModalCustomID(eventID, category.Key),
						category.Prompt, "", example, textInputStyleShort, true, potluckItemLimit),
				}}},
			},
		})
	case action == potluckNothingAction:
		note := "Saved: you're not bringing anything."
		if err := s.store.SayBringingNothing(eventID, userID); err != nil {
			note = s.potluckRefusal(err, eventID, userID)
		} else {
			s.inBackground(func() { s.syncAfterChange(eventID, nil) })
		}
		s.replyPotluckPanel(w, callbackTypeUpdateMessage, eventID, userID, note)
	case action == potluckTakeRequestAction:
		s.handleTakeRequestMenu(w, in, eventID, userID)
	case strings.HasPrefix(action, sheetRoleCardPrefix), strings.HasPrefix(action, sheetRolePanelPrefix):
		s.handleSheetRoleButton(w, action, eventID, userID)
	case action == potluckClearAction:
		note := "Cleared: you're off the sheet until you add something."
		if _, err := s.store.ClearPotluckAnswer(eventID, userID); err != nil {
			note = s.potluckRefusal(err, eventID, userID)
		} else {
			s.inBackground(func() { s.syncAfterChange(eventID, nil) })
		}
		s.replyPotluckPanel(w, callbackTypeUpdateMessage, eventID, userID, note)
	default:
		s.replyEphemeral(w, "Unknown signup action.")
	}
}

// handlePotluckModal saves what someone typed into a category's form.
func (s *Server) handlePotluckModal(w http.ResponseWriter, in *Interaction, action string, eventID int64) {
	userID, _ := in.actor()
	if userID == "" {
		s.replyEphemeral(w, "Could not read your Discord user id from that form. Nothing was changed.")
		return
	}
	category := strings.TrimPrefix(action, potluckModalPrefix)
	item, err := s.store.AddPotluckItem(eventID, userID, category, in.fieldValue(fieldPotluckItem))
	note := ""
	if err != nil {
		note = s.potluckRefusal(err, eventID, userID)
	} else {
		c, _ := potluckCategoryOf(category)
		note = fmt.Sprintf("Added: %s %s.", c.Emoji, item.Item)
		s.inBackground(func() { s.syncAfterChange(eventID, nil) })
	}
	// The form was opened from the private view, so the answer edits it.
	s.replyPotluckPanel(w, callbackTypeUpdateMessage, eventID, userID, note)
}

// potluckRefusal says plainly why the sheet did not take a change.
func (s *Server) potluckRefusal(err error, eventID int64, userID string) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "That event no longer exists."
	case errors.Is(err, ErrPotluckOff), errors.Is(err, ErrPotluckClosed):
		return "Nothing was saved: " + err.Error() + "."
	case errors.Is(err, ErrNotGoing):
		return "Nothing was saved: the sheet is for people going. Join first, then say what you're bringing."
	case errors.Is(err, ErrInvalidPotluck):
		return "Nothing was saved: " + strings.TrimPrefix(err.Error(), ErrInvalidPotluck.Error()+": ") + "."
	}
	log.Printf("[discord-signup] potluck change event=%d user=%s: %v", eventID, userID, err)
	return "Something went wrong saving that. Nothing was changed — try again."
}

// replyPotluckPanel answers with the person's private view of the sheet, as
// a new message (callbackTypeChannelMessageWithSrc) or by editing the view
// the press came from (callbackTypeUpdateMessage).
func (s *Server) replyPotluckPanel(w http.ResponseWriter, callbackType int, eventID int64, userID, note string) {
	ev, err := s.store.GetEvent(eventID)
	if errors.Is(err, ErrNotFound) {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] potluck panel event=%d: %v", eventID, err)
		s.replyEphemeral(w, "Could not read the potluck sheet. Try again.")
		return
	}
	if ev.Potluck == nil {
		s.replyEphemeral(w, "This event has no potluck sheet.")
		return
	}
	state, err := s.store.SignupState(eventID, userID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("[discord-signup] potluck panel signup event=%d user=%s: %v", eventID, userID, err)
		s.replyEphemeral(w, "Could not read whether you are going. Try again.")
		return
	}
	going := state == StateAttending
	data := potluckPanel(ev, userID, going, note)
	data["flags"] = messageFlagEphemeral
	writeJSON(w, 200, map[string]any{"type": callbackType, "data": data})
}

// potluckPanel is one person's view of an event's sheet: what is wanted,
// everyone's answers, theirs, and the buttons to change it.
func potluckPanel(ev *Event, userID string, going bool, note string) map[string]any {
	sheet := ev.Potluck
	var b strings.Builder
	fmt.Fprintf(&b, "## 🍲 Potluck — %s\n", ev.Name)
	if ev.StartsAt > 0 {
		fmt.Fprintf(&b, "🗓️ <t:%d:F>\n", ev.StartsAt)
	}
	if note != "" {
		fmt.Fprintf(&b, "\n**%s**\n", note)
	}
	if line := sheet.AskedForLine(); line != "" {
		fmt.Fprintf(&b, "\n**%s** — pick one in the menu below, or add your own.\n", line)
	}
	b.WriteString("\n" + sheet.SummaryLine() + "\n")
	if line := sheet.StillNeededLine(); line != "" {
		b.WriteString(line + "\n")
	}
	for _, c := range sheet.Counts() {
		if len(c.Items) == 0 {
			continue
		}
		parts := make([]string, len(c.Items))
		for i, it := range c.Items {
			parts[i] = fmt.Sprintf("<@%s> %s", it.DiscordUserID, it.Item)
		}
		fmt.Fprintf(&b, "%s %s\n", c.Category.Emoji, strings.Join(parts, " · "))
	}
	for _, r := range ev.Roles {
		mentions := make([]string, len(r.Members))
		for i, id := range r.Members {
			mentions[i] = "<@" + id + ">"
		}
		fmt.Fprintf(&b, "%s **%s** %s", sheetRoleEmoji, r.Name, r.Count())
		if len(mentions) > 0 {
			b.WriteString(": " + strings.Join(mentions, ", "))
		}
		b.WriteString("\n")
	}

	mine := sheet.ItemsOf(userID)
	switch {
	case !going:
		b.WriteString("\nThe sheet is for people going. Join first, then say what you're bringing.\n")
	case len(mine) > 0:
		parts := make([]string, len(mine))
		for i, it := range mine {
			c, _ := potluckCategoryOf(it.Category)
			parts[i] = c.Emoji + " " + it.Item
		}
		fmt.Fprintf(&b, "\n**You're bringing:** %s\n", strings.Join(parts, " · "))
		if len(mine) < potluckItemsPerPerson {
			fmt.Fprintf(&b, "Bringing more? Add another with the buttons below — up to %d things.\n", potluckItemsPerPerson)
		}
	case sheet.SaidNothing(userID):
		b.WriteString("\n**You said you're not bringing anything.**\n")
	default:
		b.WriteString("\n**You haven't said what you're bringing yet.** Pick a category below.\n")
	}

	content := b.String()
	if len(content) > discordMessageContentLimit {
		const notice = "\n… trimmed to fit Discord's message limit."
		content = content[:discordMessageContentLimit-len(notice)] + notice
	}
	data := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components":       []any{},
	}
	if !going || !potluckTakesAnswers(ev) {
		return data
	}
	var categoryButtons []any
	for _, c := range potluckCategories {
		categoryButtons = append(categoryButtons, map[string]any{
			"type": componentTypeButton, "style": buttonStylePrimary,
			"label": c.Label, "emoji": map[string]any{"name": c.Emoji},
			"custom_id": potluckAddCustomID(ev.ID, c.Key),
		})
	}
	answerButtons := []any{map[string]any{
		"type": componentTypeButton, "style": buttonStyleSecondary,
		"label": "Not bringing anything", "custom_id": fmt.Sprintf("%s:%s:%d", customIDPrefix, potluckNothingAction, ev.ID),
	}}
	if sheet.Answered(userID) {
		answerButtons = append(answerButtons, map[string]any{
			"type": componentTypeButton, "style": buttonStyleSecondary,
			"label": "Clear mine", "custom_id": fmt.Sprintf("%s:%s:%d", customIDPrefix, potluckClearAction, ev.ID),
		})
	}
	data["components"] = []any{
		map[string]any{"type": componentTypeActionRow, "components": categoryButtons},
		map[string]any{"type": componentTypeActionRow, "components": answerButtons},
	}
	if sheetRolesTakeAnswers(ev) && len(ev.Roles) > 0 {
		data["components"] = append(data["components"].([]any), map[string]any{"type": componentTypeActionRow,
			"components": sheetRoleButtons(ev, sheetRolePanelPrefix, userID)})
	}
	if menu := potluckRequestMenu(ev, userID); menu != nil {
		// First, so it is the first thing under the text.
		data["components"] = append([]any{menu}, data["components"].([]any)...)
	}
	return data
}

// addEventPageButton puts a link to the event's page on the web beside a
// the buttons of an event with a potluck sheet or roles, where everyone going
// can read the whole sheet: a card for a full event has room only for the
// counts. A link button rather than a link in the text, so Discord shows no
// preview. Nothing when the web pages are off or the event has neither.
func (s *Server) addEventPageButton(card map[string]any, ev *Event) {
	origin := s.webOrigin()
	if origin == "" || (!ev.PotluckEnabled && !ev.RolesEnabled) {
		return
	}
	link := map[string]any{
		"type": componentTypeButton, "style": buttonStyleLink,
		"label": "Event page", "url": fmt.Sprintf("%s/events/%d", origin, ev.ID),
	}
	rows, _ := card["components"].([]any)
	if len(rows) > 0 {
		row := rows[0].(map[string]any)
		buttons := row["components"].([]any)
		if len(buttons) < 5 {
			row["components"] = append(buttons, link)
			return
		}
	}
	card["components"] = append(rows, map[string]any{"type": componentTypeActionRow, "components": []any{link}})
}
