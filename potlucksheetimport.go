package discordsignup

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Copying an event's potluck sheet from a shared spreadsheet.
//
// A one-off, not a feature: an organiser who had already asked people to
// fill in a spreadsheet gets it copied onto the event's sheet until the event
// starts. It is switched on through the loopback API only, and no web page
// or Discord surface offers it.
//
// The spreadsheet has a header row with NAME, DRINKS, FOOD and EXPLAIN
// columns, the middle two holding TRUE or FALSE. Each row becomes at most one
// item for the person going whose name it is:
//
//   - DRINKS only, or both: a drinks item; FOOD only: a food item. One item,
//     so a row that ticks both does not count twice.
//   - The item says what EXPLAIN says, or names its categories when blank.
//   - Neither ticked: no item, so the person still counts as unanswered.
//
// A name is matched only when it is exactly one person going's short name or
// server name, ignoring case and spaces at either end. A name that fits
// nobody or several people is left for a person to sort out — it is listed in
// the import's status — never guessed.
//
// Only items this copy made (source 'sheet') are ever replaced or removed by
// it. And a row is copied again only when it changes: someone who removes a
// copied item themselves keeps it removed until they change their row.

const (
	potluckSourceSheet = "sheet"
	// actorSheetImport is who removed a copied item when its row changed.
	actorSheetImport = "sheet-import"
	// sheetFetchTimeout is how long one download of the sheet may take.
	sheetFetchTimeout = 30 * time.Second
	// sheetBodyLimit is the most of the sheet read; a party sheet is a few kB.
	sheetBodyLimit = 2 << 20
)

// ErrSheetLayout is a spreadsheet without the columns the copy reads.
var ErrSheetLayout = errors.New("the spreadsheet has no header row with NAME, DRINKS, FOOD and EXPLAIN")

// PotluckSheetImport is one event's copy from a spreadsheet, and how its
// last poll went.
type PotluckSheetImport struct {
	EventID      int64    `json:"event_id"`
	CSVURL       string   `json:"csv_url"`
	LastPolledAt int64    `json:"last_polled_at"`
	LastError    string   `json:"last_error"`
	Unmatched    []string `json:"unmatched"`
	CreatedAt    int64    `json:"created_at"`
}

// SetPotluckSheetImport starts or replaces an event's copy. The address must
// be a Google Sheets CSV export, which is what the poll knows how to read.
func (s *Store) SetPotluckSheetImport(eventID int64, csvURL string) (*PotluckSheetImport, error) {
	u, err := url.Parse(csvURL)
	if err != nil || u.Scheme != "https" || u.Host != "docs.google.com" ||
		!strings.HasPrefix(u.Path, "/spreadsheets/") || u.Query().Get("format") != "csv" {
		return nil, fmt.Errorf("%w: csv_url must be a Google Sheets export address, "+
			"https://docs.google.com/spreadsheets/d/<id>/export?format=csv", ErrInvalidPotluck)
	}
	if _, err := s.GetEvent(eventID); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`INSERT INTO potluck_sheet_imports (event_id, csv_url, created_at) VALUES (?,?,?)
		ON CONFLICT(event_id) DO UPDATE SET csv_url = excluded.csv_url, last_error = '', unmatched = '[]'`,
		eventID, csvURL, now()); err != nil {
		return nil, fmt.Errorf("save sheet import: %w", err)
	}
	return s.PotluckSheetImportOf(eventID)
}

// DeletePotluckSheetImport stops an event's copy. What it copied stays.
func (s *Store) DeletePotluckSheetImport(eventID int64) error {
	res, err := s.db.Exec(`DELETE FROM potluck_sheet_imports WHERE event_id = ?`, eventID)
	if err != nil {
		return fmt.Errorf("delete sheet import: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PotluckSheetImportOf reads one event's copy.
func (s *Store) PotluckSheetImportOf(eventID int64) (*PotluckSheetImport, error) {
	imports, err := s.potluckSheetImports(`WHERE event_id = ?`, eventID)
	if err != nil {
		return nil, err
	}
	if len(imports) == 0 {
		return nil, ErrNotFound
	}
	return &imports[0], nil
}

func (s *Store) potluckSheetImports(where string, args ...any) ([]PotluckSheetImport, error) {
	rows, err := s.db.Query(`SELECT event_id, csv_url, last_polled_at, last_error, unmatched, created_at
		FROM potluck_sheet_imports `+where+` ORDER BY event_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read sheet imports: %w", err)
	}
	defer rows.Close()
	var out []PotluckSheetImport
	for rows.Next() {
		var imp PotluckSheetImport
		var unmatched string
		if err := rows.Scan(&imp.EventID, &imp.CSVURL, &imp.LastPolledAt, &imp.LastError, &unmatched, &imp.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan sheet import: %w", err)
		}
		if err := json.Unmarshal([]byte(unmatched), &imp.Unmatched); err != nil {
			return nil, fmt.Errorf("read unmatched names of event %d: %w", imp.EventID, err)
		}
		out = append(out, imp)
	}
	return out, rows.Err()
}

func (s *Store) recordSheetPoll(eventID int64, pollErr error, unmatched []string) error {
	message := ""
	if pollErr != nil {
		message = pollErr.Error()
	}
	if unmatched == nil {
		unmatched = []string{}
	}
	names, err := json.Marshal(unmatched)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE potluck_sheet_imports SET last_polled_at = ?, last_error = ?, unmatched = ? WHERE event_id = ?`,
		now(), message, string(names), eventID)
	return err
}

// sheetRow is one person's row, as the copy reads it.
type sheetRow struct {
	Name    string
	Drinks  bool
	Food    bool
	Explain string
	// Columns is every other column's TRUE or FALSE, by roleNameKey of its
	// header, for the roles the sheet has.
	Columns map[string]bool
}

// signature is what decides whether the row's item changed since it was
// copied, and roleSignature whether its role columns did. Kept apart, so a
// role ticked or a role added does not copy the item again.
func (r sheetRow) signature() string {
	return fmt.Sprintf("%t|%t|%s", r.Drinks, r.Food, strings.Join(strings.Fields(r.Explain), " "))
}

func (r sheetRow) roleSignature(roles []SheetRole) string {
	var parts []string
	for _, role := range roles {
		if ticked, ok := r.Columns[roleNameKey(role.Name)]; ok {
			parts = append(parts, fmt.Sprintf("%d=%t", role.ID, ticked))
		}
	}
	return strings.Join(parts, "|")
}

// item is the one thing the row brings, or ok false when it ticks neither.
func (r sheetRow) item() (category, text string, ok bool) {
	switch {
	case r.Drinks && r.Food:
		category, text = PotluckDrinks, "Food and drinks"
	case r.Drinks:
		category, text = PotluckDrinks, "Drinks"
	case r.Food:
		category, text = PotluckFood, "Food"
	default:
		return "", "", false
	}
	if explain := strings.Join(strings.Fields(r.Explain), " "); explain != "" {
		text = explain
	}
	if runes := []rune(text); len(runes) > potluckItemLimit {
		text = string(runes[:potluckItemLimit-1]) + "…"
	}
	return category, text, true
}

// parsePotluckSheet reads the rows under the header row. Rows with no name
// are the sheet's empty lines.
func parsePotluckSheet(body io.Reader) ([]sheetRow, error) {
	reader := csv.NewReader(body)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read the sheet as CSV: %w", err)
	}
	header := -1
	columns := map[string]int{}
	for i, record := range records {
		found := map[string]int{}
		for j, cell := range record {
			found[strings.ToUpper(strings.TrimSpace(cell))] = j
		}
		_, name := found["NAME"]
		_, drinks := found["DRINKS"]
		_, food := found["FOOD"]
		_, explain := found["EXPLAIN"]
		if name && drinks && food && explain {
			header, columns = i, found
			break
		}
	}
	if header < 0 {
		return nil, ErrSheetLayout
	}
	cell := func(record []string, column string) string {
		if j := columns[column]; j < len(record) {
			return strings.TrimSpace(record[j])
		}
		return ""
	}
	var out []sheetRow
	for _, record := range records[header+1:] {
		name := cell(record, "NAME")
		if name == "" {
			continue
		}
		row := sheetRow{Name: name,
			Drinks:  strings.EqualFold(cell(record, "DRINKS"), "TRUE"),
			Food:    strings.EqualFold(cell(record, "FOOD"), "TRUE"),
			Explain: cell(record, "EXPLAIN"), Columns: map[string]bool{}}
		for j, heading := range records[header] {
			value := ""
			if j < len(record) {
				value = strings.TrimSpace(record[j])
			}
			if strings.EqualFold(value, "TRUE") || strings.EqualFold(value, "FALSE") {
				row.Columns[roleNameKey(heading)] = strings.EqualFold(value, "TRUE")
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// matchGoing finds the one person going a sheet name belongs to.
func matchGoing(name string, going []Signup) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	var found []string
	for _, sg := range going {
		if strings.ToLower(strings.TrimSpace(sg.ReadableName)) == want ||
			strings.ToLower(strings.TrimSpace(sg.DisplayName)) == want {
			found = append(found, sg.DiscordUserID)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// copySheetRow brings one person's copied item in line with their row, in
// one transaction: the items this copy made before come off, the new one
// goes on, and the row's signature is kept. Returns whether anything changed.
func (s *Store) copySheetRow(eventID int64, userID string, row sheetRow, roles []SheetRole) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	var lastItem, lastRoles string
	err = tx.QueryRow(`SELECT row_signature, role_signature FROM potluck_sheet_rows WHERE event_id = ? AND discord_user_id = ?`,
		eventID, userID).Scan(&lastItem, &lastRoles)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read copied row: %w", err)
	}
	itemChanged, rolesChanged := lastItem != row.signature(), lastRoles != row.roleSignature(roles)
	if !itemChanged && !rolesChanged {
		return false, nil
	}
	ts := now()
	if itemChanged {
		if _, err := tx.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
			WHERE event_id = ? AND discord_user_id = ? AND source = ? AND removed_at = 0`,
			ts, actorSheetImport, eventID, userID, potluckSourceSheet); err != nil {
			return false, fmt.Errorf("take off copied items: %w", err)
		}
		if category, text, ok := row.item(); ok {
			// Saying what they bring ends "bringing nothing", as it does when
			// they say it themselves.
			if _, err := tx.Exec(`UPDATE potluck_items SET removed_at = ?, removed_by = ?
				WHERE event_id = ? AND discord_user_id = ? AND category = ? AND removed_at = 0`,
				ts, actorSheetImport, eventID, userID, potluckNothing); err != nil {
				return false, fmt.Errorf("clear bringing nothing: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO potluck_items (event_id, discord_user_id, category, item, added_at, source)
				VALUES (?,?,?,?,?,?)`, eventID, userID, category, text, ts, potluckSourceSheet); err != nil {
				return false, fmt.Errorf("copy item: %w", err)
			}
		}
	}
	// Each role with a column: ticked puts them in it, unticked takes them
	// out of it if this copy put them there. A full role is left as it is.
	for _, role := range roles {
		ticked, ok := row.Columns[roleNameKey(role.Name)]
		if !ok || !rolesChanged {
			continue
		}
		var copied, own int
		if err := tx.QueryRow(`SELECT COUNT(*) FILTER (WHERE source = ?), COUNT(*) FILTER (WHERE source != ?)
			FROM sheet_role_signups WHERE role_id = ? AND discord_user_id = ? AND left_at = 0`,
			potluckSourceSheet, potluckSourceSheet, role.ID, userID).Scan(&copied, &own); err != nil {
			return false, fmt.Errorf("read role of copied row: %w", err)
		}
		switch {
		case ticked && copied+own == 0:
			err := joinSheetRoleTx(tx, eventID, role.ID, role.Capacity, userID, potluckSourceSheet, ts)
			if errors.Is(err, ErrSheetRoleFull) {
				log.Printf("[discord-signup] sheet copy of %s for event %d: %s is full", userID, eventID, role.Name)
			} else if err != nil {
				return false, err
			}
		case !ticked && copied > 0:
			if _, err := tx.Exec(`UPDATE sheet_role_signups SET left_at = ?, left_by = ?
				WHERE role_id = ? AND discord_user_id = ? AND source = ? AND left_at = 0`,
				ts, actorSheetImport, role.ID, userID, potluckSourceSheet); err != nil {
				return false, fmt.Errorf("leave copied role: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO potluck_sheet_rows (event_id, discord_user_id, row_signature, role_signature, copied_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(event_id, discord_user_id) DO UPDATE SET row_signature = excluded.row_signature,
			role_signature = excluded.role_signature, copied_at = excluded.copied_at`,
		eventID, userID, row.signature(), row.roleSignature(roles), ts); err != nil {
		return false, fmt.Errorf("keep copied row: %w", err)
	}
	return true, tx.Commit()
}

// PollPotluckSheets copies every sheet whose event has not started. Run
// every few minutes by the scheduler. A sheet that cannot be read is
// recorded on its import and the others still run.
func (s *Server) PollPotluckSheets() (changed int, err error) {
	imports, err := s.store.potluckSheetImports(``)
	if err != nil {
		return 0, err
	}
	for _, imp := range imports {
		n, pollErr := s.pollPotluckSheet(imp)
		changed += n
		if pollErr != nil {
			log.Printf("[discord-signup] potluck sheet for event %d: %v", imp.EventID, pollErr)
		}
	}
	return changed, nil
}

func (s *Server) pollPotluckSheet(imp PotluckSheetImport) (int, error) {
	ev, err := s.store.GetEvent(imp.EventID)
	if err != nil {
		return 0, err
	}
	if ev.StartsAt <= now() || !potluckTakesAnswers(ev) {
		return 0, nil // the event has started, or has no sheet: nothing left to copy
	}
	rows, err := fetchPotluckSheet(imp.CSVURL)
	if err != nil {
		return 0, errors.Join(err, s.store.recordSheetPoll(ev.ID, err, imp.Unmatched))
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		return 0, err
	}
	var going []Signup
	for _, sg := range roster {
		if sg.State == StateAttending {
			going = append(going, sg)
		}
	}
	changed := 0
	unmatched := []string{}
	for _, row := range rows {
		userID, ok := matchGoing(row.Name, going)
		if !ok {
			unmatched = append(unmatched, row.Name)
			continue
		}
		did, err := s.store.copySheetRow(ev.ID, userID, row, ev.Potluck.Roles)
		if err != nil {
			return changed, errors.Join(err, s.store.recordSheetPoll(ev.ID, err, unmatched))
		}
		if did {
			changed++
		}
	}
	if err := s.store.recordSheetPoll(ev.ID, nil, unmatched); err != nil {
		return changed, err
	}
	if changed > 0 {
		s.inBackground(func() { s.syncAfterChange(ev.ID, nil) })
	}
	return changed, nil
}

func fetchPotluckSheet(csvURL string) ([]sheetRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sheetFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, csvURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download the sheet: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download the sheet: %s (is it shared as anyone with the link can view?)", resp.Status)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") {
		return nil, fmt.Errorf("download the sheet: got %q, not CSV (is it shared as anyone with the link can view?)",
			resp.Header.Get("Content-Type"))
	}
	return parsePotluckSheet(io.LimitReader(resp.Body, sheetBodyLimit))
}

// Loopback routes. No web page links them; /api/ is never proxied.

func (s *Server) handleGetPotluckSheetImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	imp, err := s.store.PotluckSheetImportOf(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imp)
}

func (s *Server) handlePutPotluckSheetImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	var body struct {
		CSVURL string `json:"csv_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"csv_url\": …}"})
		return
	}
	imp, err := s.store.SetPotluckSheetImport(id, body.CSVURL)
	if errors.Is(err, ErrInvalidPotluck) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imp)
}

func (s *Server) handleDeletePotluckSheetImport(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}
	if err := s.store.DeletePotluckSheetImport(id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePollPotluckSheets(w http.ResponseWriter, r *http.Request) {
	changed, err := s.PollPotluckSheets()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}
