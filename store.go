// Package discordsignup owns event signup rosters with a capacity and a
// waitlist — the thing Discord's own scheduled events cannot do.
//
// Discord's "Interested" button is a notification subscription, not a seat
// reservation: the scheduled event object has no capacity field, and the API
// offers no way to remove a subscriber. So a roster with a cap cannot be built
// on top of it and has to live here, with the Discord side reduced to a link.
package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrNotFound is returned when a lookup finds no row.
var ErrNotFound = errors.New("not found")

// ErrInvalidEvent marks an event the caller described wrongly — no name, no
// guild, a status outside the vocabulary — so the HTTP layer answers 400
// rather than 500.
var ErrInvalidEvent = errors.New("invalid event")

// ErrEventNotOpen is returned when someone tries to join an event that is
// closed or cancelled. Distinct from ErrInvalidEvent because the request was
// well formed and the answer is about timing, which is what the person on the
// other end needs to be told.
var ErrEventNotOpen = errors.New("event is not open for signups")

// Store is the roster database.
type Store struct {
	db      *sql.DB
	dataDir string
}

// Event is one signup roster.
type Event struct {
	ID                      int64  `json:"id"`
	GuildID                 string `json:"guild_id"`
	ChannelID               string `json:"channel_id"`
	MessageID               string `json:"message_id"`
	DiscordScheduledEventID string `json:"discord_scheduled_event_id"`
	Name                    string `json:"name"`
	Description             string `json:"description"`
	Capacity                int    `json:"capacity"` // 0 = unlimited
	Status                  string `json:"status"`
	AttendingRoleID         string `json:"attending_role_id"`
	WaitlistRoleID          string `json:"waitlist_role_id"`
	StartsAt                int64  `json:"starts_at"`
	EndsAt                  int64  `json:"ends_at"`
	// Location is free text for an EXTERNAL event, and empty for a voice or
	// stage one where the channel is the location.
	Location   string `json:"location"`
	EntityType string `json:"entity_type"`
	// RecurrenceRule is an RFC 5545 RRULE. Discord accepts a subset of the same
	// grammar on its native events, so one rule drives both.
	RecurrenceRule string `json:"recurrence_rule"`
	// Timezone is an IANA zone name and is required whenever RecurrenceRule is
	// set. Never defaulted — see the note on the column.
	Timezone string `json:"timezone"`
	Origin   string `json:"origin"`
	// DiscordInterestedCount is Discord's own number, carried for display
	// beside ours. It is never the roster and never gates a capacity decision.
	DiscordInterestedCount int    `json:"discord_interested_count"`
	DiscordSyncedAt        int64  `json:"discord_synced_at"`
	CreatedBy              string `json:"created_by"`
	CreatedAt              int64  `json:"created_at"`
	UpdatedAt              int64  `json:"updated_at"`

	// AttendingCount and WaitlistCount are computed by the read path. They are
	// never stored, so they can never disagree with the signups table.
	AttendingCount int `json:"attending_count"`
	WaitlistCount  int `json:"waitlist_count"`
}

// Signup is one person's place on one roster.
type Signup struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	DiscordUserID string `json:"discord_user_id"`
	// DisplayName is display only. Never join on it.
	DisplayName    string `json:"display_name"`
	Position       int    `json:"position"`
	State          string `json:"state"`
	SignedUpAt     int64  `json:"signed_up_at"`
	StateChangedAt int64  `json:"state_changed_at"`
	// JoinedVia records which door they came in through. It decides what
	// un-marking Interested on Discord does to them.
	JoinedVia string `json:"joined_via"`
	// DiscordInterested is what Discord currently says, not what this roster
	// says. The two can disagree permanently and there is no API to reconcile
	// them, so both are kept rather than one being derived from the other.
	DiscordInterested bool `json:"discord_interested"`

	// WaitlistPlace is 1 for the next person in line, 2 for the one after, and
	// 0 for anyone not waitlisted. Computed on read from position ordering, so
	// it stays true after a promotion without anything being renumbered.
	WaitlistPlace int `json:"waitlist_place"`
}

// Transition is one row of the append-only history.
type Transition struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	DiscordUserID string `json:"discord_user_id"`
	Action        string `json:"action"`
	FromState     string `json:"from_state"`
	ToState       string `json:"to_state"`
	Position      int    `json:"position"`
	Actor         string `json:"actor"`
	At            int64  `json:"at"`
	// DisplayName is joined on read from the signup row, never stored on this
	// one. A transition is a fact about an id; the name is how that id looked
	// when somebody last saw it, and copying it here would freeze a stale
	// spelling into an append-only table that can never be corrected.
	DisplayName string `json:"display_name"`
}

// JoinResult is what happened when someone pressed Join.
type JoinResult struct {
	Signup Signup `json:"signup"`
	// AlreadySignedUp is true when they already held this exact place and
	// nothing changed. The button is idempotent — a double click must not cost
	// someone their position or write a second history row.
	AlreadySignedUp bool `json:"already_signed_up"`
}

// LeaveResult is what happened when someone left, including the knock-on.
type LeaveResult struct {
	Signup Signup `json:"signup"`
	// Promoted is the person moved off the waitlist into the freed place, or
	// nil if there was no waitlist. The caller is responsible for telling them
	// — this store records the promotion, it does not deliver the news.
	Promoted *Signup `json:"promoted,omitempty"`
}

// Open opens or creates the roster database. dataDir defaults to
// ~/.config/discord-signup-store.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home: %w", err)
		}
		dataDir = filepath.Join(home, ".config", "discord-signup-store")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "discord-signup-store.db")

	// _txlock=immediate is the whole concurrency design, so it is not a detail.
	//
	// SQLite's default deferred transaction takes its write lock at the first
	// write, which is AFTER the capacity check has read the count. Two people
	// pressing Join on the last place would both read 19-of-20, both decide
	// "there is room", and the second would get SQLITE_BUSY on upgrade — or,
	// worse, succeed and leave 21 people attending. BEGIN IMMEDIATE takes the
	// write lock up front, so the check and the insert are one atomic decision
	// and the second caller waits and then correctly reads 20-of-20.
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

// addedColumn is one column that arrived after a table already existed on some
// host. schema.sql is create-only, so editing it does nothing for a database
// that already has the table — these run as ALTERs instead.
type addedColumn struct {
	table  string
	column string
	// ddl is the column definition, minus the name. Every one of these must
	// carry a DEFAULT: SQLite refuses to add a NOT NULL column without one, and
	// a nullable column would put a NULL into a Go string scan and panic.
	ddl string
}

// columnsAddedAfterFirstRelease grows over time and is never reordered or
// pruned. A host that has been running since before any given line still needs
// that line to run.
var columnsAddedAfterFirstRelease = []addedColumn{
	// Everything Discord's own scheduled event carries that a locally-created
	// roster also wants, so an imported event and a hand-made one are the same
	// kind of row rather than two shapes the UI has to branch on.
	{"events", "ends_at", "INTEGER NOT NULL DEFAULT 0"},
	{"events", "location", "TEXT NOT NULL DEFAULT ''"},
	{"events", "entity_type", "TEXT NOT NULL DEFAULT ''"},
	{"events", "recurrence_rule", "TEXT NOT NULL DEFAULT ''"},
	// An IANA zone name, never a UTC offset. An offset cannot know what happens
	// at a DST transition, so a rule anchored to one silently drifts an hour
	// twice a year. Mandatory whenever recurrence_rule is set.
	{"events", "timezone", "TEXT NOT NULL DEFAULT ''"},
	// 'local' (created here) or 'discord' (imported from a native scheduled
	// event). Not derivable from discord_scheduled_event_id being set, because
	// a locally-created event can be published to Discord and then also carries
	// one — origin records who owns the thing, not whether a link exists.
	{"events", "origin", "TEXT NOT NULL DEFAULT 'local'"},
	// Discord's own Interested count, stored so the UI can show it next to our
	// roster and label both. It is NEVER the roster and never feeds a capacity
	// decision — Discord cannot cap it and cannot remove anyone from it.
	{"events", "discord_interested_count", "INTEGER NOT NULL DEFAULT 0"},
	{"events", "discord_synced_at", "INTEGER NOT NULL DEFAULT 0"},
	{"events", "created_by", "TEXT NOT NULL DEFAULT ''"},

	// How this person got onto the roster. Not cosmetic: it decides what
	// un-marking Interested on Discord does to them. Someone who pressed Join
	// keeps their place regardless of their Discord RSVP; someone who only ever
	// clicked Interested came in through that door and leaves through it.
	{"signups", "joined_via", "TEXT NOT NULL DEFAULT 'button'"},

	// Whether Discord currently lists this person as Interested in the linked
	// scheduled event.
	//
	// Tracked separately from state because the two can disagree and there is
	// no way to make them agree: Discord has no endpoint to remove a
	// subscriber, so someone who presses Leave here stays Interested there
	// forever. Without this column, the next Interested signal would read as
	// fresh intent and put them straight back on a roster they chose to leave.
	{"signups", "discord_interested", "INTEGER NOT NULL DEFAULT 0"},
}

// ensureColumns applies every ALTER that a pre-existing database is missing.
//
// Adding a column that is already there is an error in SQLite, not a no-op, so
// each one is checked against the live table first. A failure here is fatal at
// boot rather than swallowed: a service running against a database missing a
// column it reads would fail later, on a request, somewhere less obvious.
func ensureColumns(db *sql.DB) error {
	for _, c := range columnsAddedAfterFirstRelease {
		have, err := columnExists(db, c.table, c.column)
		if err != nil {
			return err
		}
		if have {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.ddl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
		}
		log.Printf("[discord-signup] added column %s.%s", c.table, c.column)
	}
	return nil
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir reports where the database lives.
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

// ---------------------------------------------------------------- events

// CreateEvent stores a new roster.
func (s *Store) CreateEvent(e Event) (*Event, error) {
	e.Name = strings.TrimSpace(e.Name)
	e.GuildID = strings.TrimSpace(e.GuildID)
	e.ChannelID = strings.TrimSpace(e.ChannelID)
	if e.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidEvent)
	}
	if e.GuildID == "" {
		return nil, fmt.Errorf("%w: guild_id is required", ErrInvalidEvent)
	}
	if e.ChannelID == "" {
		return nil, fmt.Errorf("%w: channel_id is required", ErrInvalidEvent)
	}
	if e.Capacity < 0 {
		return nil, fmt.Errorf("%w: capacity cannot be negative (0 means unlimited)", ErrInvalidEvent)
	}
	if e.Status == "" {
		e.Status = StatusOpen
	}
	if !validStatuses[e.Status] {
		return nil, fmt.Errorf("%w: status %q is not one of %v", ErrInvalidEvent, e.Status, ValidStatuses())
	}
	if e.Origin == "" {
		e.Origin = OriginLocal
	}
	if !validOrigins[e.Origin] {
		return nil, fmt.Errorf("%w: origin %q is not one of %v", ErrInvalidEvent, e.Origin, ValidOrigins())
	}
	if err := validateRecurrence(e.RecurrenceRule, e.Timezone); err != nil {
		return nil, err
	}
	if e.EndsAt != 0 && e.StartsAt != 0 && e.EndsAt < e.StartsAt {
		return nil, fmt.Errorf("%w: ends_at is before starts_at", ErrInvalidEvent)
	}

	ts := now()
	res, err := s.db.Exec(`
		INSERT INTO events (guild_id, channel_id, message_id, discord_scheduled_event_id,
		                    name, description, capacity, status,
		                    attending_role_id, waitlist_role_id, starts_at, ends_at,
		                    location, entity_type, recurrence_rule, timezone, origin,
		                    discord_interested_count, discord_synced_at, created_by,
		                    created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.GuildID, e.ChannelID, e.MessageID, e.DiscordScheduledEventID,
		e.Name, e.Description, e.Capacity, e.Status,
		e.AttendingRoleID, e.WaitlistRoleID, e.StartsAt, e.EndsAt,
		e.Location, e.EntityType, e.RecurrenceRule, e.Timezone, e.Origin,
		e.DiscordInterestedCount, e.DiscordSyncedAt, e.CreatedBy, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read inserted id: %w", err)
	}
	return s.GetEvent(id)
}

const eventColumns = `id, guild_id, channel_id, message_id, discord_scheduled_event_id,
	name, description, capacity, status, attending_role_id, waitlist_role_id,
	starts_at, ends_at, location, entity_type, recurrence_rule, timezone, origin,
	discord_interested_count, discord_synced_at, created_by, created_at, updated_at`

func scanEvent(sc interface{ Scan(...any) error }) (*Event, error) {
	var e Event
	err := sc.Scan(&e.ID, &e.GuildID, &e.ChannelID, &e.MessageID, &e.DiscordScheduledEventID,
		&e.Name, &e.Description, &e.Capacity, &e.Status, &e.AttendingRoleID, &e.WaitlistRoleID,
		&e.StartsAt, &e.EndsAt, &e.Location, &e.EntityType, &e.RecurrenceRule, &e.Timezone,
		&e.Origin, &e.DiscordInterestedCount, &e.DiscordSyncedAt, &e.CreatedBy,
		&e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// GetEvent returns one roster with its live counts.
func (s *Store) GetEvent(id int64) (*Event, error) {
	row := s.db.QueryRow(`SELECT `+eventColumns+` FROM events WHERE id = ? AND deleted_at = 0`, id)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}
	if err := s.fillCounts(e); err != nil {
		return nil, err
	}
	return e, nil
}

// EventByDiscordScheduledEventID finds the local roster linked to a native
// Discord scheduled event. This is the join the importer uses, and it is on the
// id Discord assigned — never on the name, which two events can share.
func (s *Store) EventByDiscordScheduledEventID(discordEventID string) (*Event, error) {
	row := s.db.QueryRow(`SELECT `+eventColumns+`
		FROM events WHERE discord_scheduled_event_id = ? AND deleted_at = 0`, discordEventID)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}
	if err := s.fillCounts(e); err != nil {
		return nil, err
	}
	return e, nil
}

// EventByMessage finds the roster a signup message belongs to. This is the
// lookup the interaction handler uses, because a button click arrives carrying
// the message it sits on.
func (s *Store) EventByMessage(messageID string) (*Event, error) {
	row := s.db.QueryRow(`SELECT `+eventColumns+` FROM events WHERE message_id = ? AND deleted_at = 0`, messageID)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}
	if err := s.fillCounts(e); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Store) fillCounts(e *Event) error {
	err := s.db.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE state = ?),
			COUNT(*) FILTER (WHERE state = ?)
		FROM signups WHERE event_id = ?`,
		StateAttending, StateWaitlisted, e.ID,
	).Scan(&e.AttendingCount, &e.WaitlistCount)
	if err != nil {
		return fmt.Errorf("count signups: %w", err)
	}
	return nil
}

// ListEvents returns rosters, newest first. guildID and status are optional
// filters; empty means no filter.
func (s *Store) ListEvents(guildID, status string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + eventColumns + ` FROM events WHERE deleted_at = 0`
	args := []any{}
	if guildID != "" {
		query += ` AND guild_id = ?`
		args = append(args, guildID)
	}
	if status != "" {
		if !validStatuses[status] {
			return nil, fmt.Errorf("%w: status %q is not one of %v", ErrInvalidEvent, status, ValidStatuses())
		}
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	for i := range out {
		if err := s.fillCounts(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateEvent applies the non-nil fields of patch.
//
// Lowering capacity below the number already attending is allowed and does NOT
// demote anyone: those people were told they had a place. The cap governs the
// next join, so the roster simply stops admitting until attrition brings it
// back under. Silently demoting someone who is holding a confirmation message
// would make that message a lie.
func (s *Store) UpdateEvent(id int64, patch EventPatch) (*Event, error) {
	existing, err := s.GetEvent(id)
	if err != nil {
		return nil, err
	}
	sets := []string{}
	args := []any{}
	add := func(col string, val any) { sets = append(sets, col+" = ?"); args = append(args, val) }

	if patch.Name != nil {
		if strings.TrimSpace(*patch.Name) == "" {
			return nil, fmt.Errorf("%w: name cannot be blank", ErrInvalidEvent)
		}
		add("name", strings.TrimSpace(*patch.Name))
	}
	if patch.Description != nil {
		add("description", *patch.Description)
	}
	if patch.Capacity != nil {
		if *patch.Capacity < 0 {
			return nil, fmt.Errorf("%w: capacity cannot be negative (0 means unlimited)", ErrInvalidEvent)
		}
		add("capacity", *patch.Capacity)
	}
	if patch.Status != nil {
		if !validStatuses[*patch.Status] {
			return nil, fmt.Errorf("%w: status %q is not one of %v", ErrInvalidEvent, *patch.Status, ValidStatuses())
		}
		add("status", *patch.Status)
	}
	if patch.MessageID != nil {
		add("message_id", *patch.MessageID)
	}
	if patch.ChannelID != nil {
		add("channel_id", *patch.ChannelID)
	}
	if patch.DiscordScheduledEventID != nil {
		add("discord_scheduled_event_id", *patch.DiscordScheduledEventID)
	}
	if patch.AttendingRoleID != nil {
		add("attending_role_id", *patch.AttendingRoleID)
	}
	if patch.WaitlistRoleID != nil {
		add("waitlist_role_id", *patch.WaitlistRoleID)
	}
	if patch.StartsAt != nil {
		add("starts_at", *patch.StartsAt)
	}
	if patch.EndsAt != nil {
		add("ends_at", *patch.EndsAt)
	}
	if patch.Location != nil {
		add("location", *patch.Location)
	}
	if patch.EntityType != nil {
		add("entity_type", *patch.EntityType)
	}
	if patch.RecurrenceRule != nil || patch.Timezone != nil {
		// Validated as a pair against the values the row will END UP with, not
		// against what the request happened to mention. Patching a rule onto an
		// event that already has a zone must pass; clearing the zone from under
		// a live rule must not.
		rule, zone := existing.RecurrenceRule, existing.Timezone
		if patch.RecurrenceRule != nil {
			rule = *patch.RecurrenceRule
		}
		if patch.Timezone != nil {
			zone = *patch.Timezone
		}
		if err := validateRecurrence(rule, zone); err != nil {
			return nil, err
		}
		add("recurrence_rule", rule)
		add("timezone", zone)
	}
	if patch.DiscordInterestedCount != nil {
		add("discord_interested_count", *patch.DiscordInterestedCount)
		add("discord_synced_at", now())
	}
	if len(sets) == 0 {
		return existing, nil
	}
	add("updated_at", now())
	args = append(args, id)

	if _, err := s.db.Exec(`UPDATE events SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return nil, fmt.Errorf("update event: %w", err)
	}
	return s.GetEvent(id)
}

// EventPatch carries a partial update. Every field is a pointer so that
// "absent" and "set to the zero value" stay distinguishable — without that,
// omitting capacity from a PATCH body would silently reset it to unlimited.
type EventPatch struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Capacity    *int    `json:"capacity"`
	Status      *string `json:"status"`
	MessageID   *string `json:"message_id"`
	// ChannelID moves with the card. It changes exactly once in an event's
	// life, when the event finishes and its card is reposted to past events.
	ChannelID               *string `json:"channel_id"`
	DiscordScheduledEventID *string `json:"discord_scheduled_event_id"`
	AttendingRoleID         *string `json:"attending_role_id"`
	WaitlistRoleID          *string `json:"waitlist_role_id"`
	StartsAt                *int64  `json:"starts_at"`
	EndsAt                  *int64  `json:"ends_at"`
	Location                *string `json:"location"`
	EntityType              *string `json:"entity_type"`
	RecurrenceRule          *string `json:"recurrence_rule"`
	Timezone                *string `json:"timezone"`
	DiscordInterestedCount  *int    `json:"discord_interested_count"`
}

// validateRecurrence enforces the one rule that cannot be defaulted: a
// recurrence rule without a zone is a rule that drifts an hour at every DST
// transition, so it is refused at write time rather than left to surprise
// someone in November.
func validateRecurrence(rule, timezone string) error {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return nil
	}
	if strings.TrimSpace(timezone) == "" {
		return fmt.Errorf("%w: recurrence_rule needs a timezone (an IANA name like "+
			"\"America/Los_Angeles\", never a UTC offset)", ErrInvalidEvent)
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("%w: timezone %q is not an IANA zone name: %v", ErrInvalidEvent, timezone, err)
	}
	if !strings.Contains(strings.ToUpper(rule), "FREQ=") {
		return fmt.Errorf("%w: recurrence_rule %q is not an RRULE (it needs FREQ=)", ErrInvalidEvent, rule)
	}
	return nil
}

// DeleteEvent soft-deletes a roster. The rows stay so the history stays
// readable; use status 'cancelled' if you mean the event is off but the record
// should remain visible.
func (s *Store) DeleteEvent(id int64) error {
	res, err := s.db.Exec(`UPDATE events SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at = 0`, now(), now(), id)
	if err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("read rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
