package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	adksession "google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var _ adksession.Service = (*AdkSessionService)(nil)

// AdkSessionService implements ADK's session.Service backed by the project's
// SQLite store. It shares the same *sql.DB and migration regime.
type AdkSessionService struct {
	db *sql.DB
}

// NewAdkSessionService creates a durable ADK session service from the
// project's managed SQLite Store. The caller must ensure the database is
// already migrated to at least v10.
func NewAdkSessionService(store *Store) *AdkSessionService {
	if store == nil || store.db == nil {
		return nil
	}
	return &AdkSessionService{db: store.db}
}

// --- session.Service implementation ---

func (s *AdkSessionService) Create(ctx context.Context, req *adksession.CreateRequest) (*adksession.CreateResponse, error) {
	if req.AppName == "" || req.UserID == "" {
		return nil, fmt.Errorf("app_name and user_id are required")
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = platform.NewUUID(ctx)
	}

	state := req.State
	if state == nil {
		state = make(map[string]any)
	}

	now := platform.Now(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.upsertAppState(ctx, tx, req.AppName); err != nil {
		return nil, fmt.Errorf("ensure app state: %w", err)
	}
	if err := s.upsertUserState(ctx, tx, req.AppName, req.UserID); err != nil {
		return nil, fmt.Errorf("ensure user state: %w", err)
	}

	appDelta, userDelta, sessionState := extractStateDeltas(state)

	if len(appDelta) > 0 {
		if err := s.applyStateDelta(ctx, tx, "adk_app_state", "app_name", req.AppName, appDelta, now); err != nil {
			return nil, fmt.Errorf("apply app state delta: %w", err)
		}
	}
	if len(userDelta) > 0 {
		if err := s.applyUserStateDelta(ctx, tx, req.AppName, req.UserID, userDelta, now); err != nil {
			return nil, fmt.Errorf("apply user state delta: %w", err)
		}
	}
	appState, err := s.appState(ctx, tx, req.AppName)
	if err != nil {
		return nil, fmt.Errorf("read app state: %w", err)
	}
	userState, err := s.userState(ctx, tx, req.AppName, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("read user state: %w", err)
	}

	stateJSON, err := json.Marshal(sessionState)
	if err != nil {
		return nil, fmt.Errorf("marshal session state: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO adk_sessions (app_name, user_id, session_id, state, create_time, update_time)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		req.AppName, req.UserID, sessionID, string(stateJSON), now.UnixMicro(), now.UnixMicro(),
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create session: %w", err)
	}

	sess := newLocalSession(req.AppName, req.UserID, sessionID, now)
	sess.state = mergeStates(appState, userState, sessionState)
	return &adksession.CreateResponse{Session: sess}, nil
}

func (s *AdkSessionService) Get(ctx context.Context, req *adksession.GetRequest) (*adksession.GetResponse, error) {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return nil, fmt.Errorf("app_name, user_id, session_id are required")
	}

	var stateJSON string
	var updateMicro, revision int64
	err := s.db.QueryRowContext(ctx,
		`SELECT state, update_time, revision FROM adk_sessions WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		req.AppName, req.UserID, req.SessionID,
	).Scan(&stateJSON, &updateMicro, &revision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("session %q not found", req.SessionID)
		}
		return nil, fmt.Errorf("fetch session: %w", err)
	}

	var sessionState map[string]any
	if err := json.Unmarshal([]byte(stateJSON), &sessionState); err != nil {
		return nil, fmt.Errorf("unmarshal session state: %w", err)
	}

	updatedAt := time.UnixMicro(updateMicro)
	appState, err := s.appState(ctx, s.db, req.AppName)
	if err != nil {
		return nil, fmt.Errorf("read app state: %w", err)
	}
	userState, err := s.userState(ctx, s.db, req.AppName, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("read user state: %w", err)
	}
	sess := newLocalSession(req.AppName, req.UserID, req.SessionID, updatedAt)
	sess.state = mergeStates(appState, userState, sessionState)
	sess.revision = revision

	// Read scoped state before opening the event cursor. The Store uses one
	// SQLite connection, so a second query while rows are open would deadlock.

	// Load events
	query := `SELECT id, invocation_id, author, actions, long_running_tool_ids, routes, output,
		node_info, requested_input, branch, isolation_scope, timestamp, content,
		grounding_metadata, custom_metadata, usage_metadata, citation_metadata,
		error_code, error_message, partial, turn_complete, interrupted
		FROM adk_events
		WHERE app_name = ? AND user_id = ? AND session_id = ?
		ORDER BY ordinal DESC`

	if req.NumRecentEvents > 0 {
		query += fmt.Sprintf(" LIMIT %d", req.NumRecentEvents)
	}

	rows, err := s.db.QueryContext(ctx, query, req.AppName, req.UserID, req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("fetch events: %w", err)
	}
	defer rows.Close()

	loadedEvents, err := scanEvents(rows, req.After)
	if err != nil {
		return nil, err
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}

	// Events were fetched in DESC order; reverse to ASC.
	reverseEvents(loadedEvents)
	sess.events = loadedEvents

	return &adksession.GetResponse{Session: sess}, nil
}

func (s *AdkSessionService) List(ctx context.Context, req *adksession.ListRequest) (*adksession.ListResponse, error) {
	if req.AppName == "" {
		return nil, fmt.Errorf("app_name is required")
	}

	appState, err := s.appState(ctx, s.db, req.AppName)
	if err != nil {
		return nil, fmt.Errorf("read app state: %w", err)
	}

	query := `SELECT s.app_name, s.user_id, s.session_id, s.state, s.create_time, s.update_time, s.revision, u.state
		FROM adk_sessions AS s
		JOIN adk_user_state AS u ON u.app_name = s.app_name AND u.user_id = s.user_id
		WHERE s.app_name = ?`
	args := []any{req.AppName}

	if req.UserID != "" {
		query += ` AND s.user_id = ?`
		args = append(args, req.UserID)
	}
	query += ` ORDER BY s.update_time DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []adksession.Session
	for rows.Next() {
		var appName, userID, sessionID, stateJSON, userStateJSON string
		var createTime, updateMicro, revision int64
		if err := rows.Scan(&appName, &userID, &sessionID, &stateJSON, &createTime, &updateMicro, &revision, &userStateJSON); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
			return nil, fmt.Errorf("unmarshal state: %w", err)
		}
		var userState map[string]any
		if err := json.Unmarshal([]byte(userStateJSON), &userState); err != nil {
			return nil, fmt.Errorf("unmarshal user state: %w", err)
		}
		sess := newLocalSession(appName, userID, sessionID, time.UnixMicro(updateMicro))
		sess.state = mergeStates(appState, userState, state)
		sess.revision = revision
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}

	return &adksession.ListResponse{Sessions: sessions}, nil
}

// RootSessionProviderFamilies returns the provider-family marker of every
// durable ADK session, keyed by session ID. Sessions without the marker are
// classified as legacy openai_compatible sessions.
func (s *AdkSessionService) RootSessionProviderFamilies(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, state FROM adk_sessions`)
	if err != nil {
		return nil, fmt.Errorf("list session provider families: %w", err)
	}
	defer rows.Close()

	families := make(map[string]string)
	for rows.Next() {
		var sessionID, stateJSON string
		if err := rows.Scan(&sessionID, &stateJSON); err != nil {
			return nil, fmt.Errorf("scan session provider family: %w", err)
		}
		family := domain.ProviderFamilyOpenAICompatible
		var state map[string]any
		if err := json.Unmarshal([]byte(stateJSON), &state); err == nil {
			if value, ok := state[domain.ProviderFamilyStateKey].(string); ok && strings.TrimSpace(value) != "" {
				family = value
			}
		}
		families[sessionID] = family
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session provider families: %w", err)
	}
	return families, nil
}

func (s *AdkSessionService) Delete(ctx context.Context, req *adksession.DeleteRequest) error {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return fmt.Errorf("app_name, user_id, session_id are required")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM adk_sessions WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		req.AppName, req.UserID, req.SessionID,
	)
	return err
}

func (s *AdkSessionService) AppendEvent(ctx context.Context, curSession adksession.Session, event *adksession.Event) error {
	if curSession == nil {
		return fmt.Errorf("session is nil")
	}
	if event == nil {
		return fmt.Errorf("event is nil")
	}
	if event.Partial {
		return nil
	}

	ls, ok := curSession.(*localSession)
	if !ok {
		return fmt.Errorf("unexpected session type %T", curSession)
	}

	event.Timestamp = event.Timestamp.Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Verify the session exists and is not stale.
	var storageUpdateMicro, storageRevision int64
	err = tx.QueryRowContext(ctx,
		`SELECT update_time, revision FROM adk_sessions WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		ls.appName, ls.userID, ls.sessionID,
	).Scan(&storageUpdateMicro, &storageRevision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session not found, cannot apply event")
		}
		return fmt.Errorf("fetch session for append: %w", err)
	}

	if storageRevision != ls.revision {
		return fmt.Errorf(
			"stale session error: local revision (%d) differs from database (%d)",
			ls.revision, storageRevision,
		)
	}

	// Apply state deltas.
	now := time.Now().UTC()
	appDelta, userDelta, sessionDelta := extractStateDeltas(event.Actions.StateDelta)

	if len(appDelta) > 0 {
		if err := s.applyStateDelta(ctx, tx, "adk_app_state", "app_name", ls.appName, appDelta, now); err != nil {
			return fmt.Errorf("apply app state delta: %w", err)
		}
	}
	if len(userDelta) > 0 {
		if err := s.applyUserStateDelta(ctx, tx, ls.appName, ls.userID, userDelta, now); err != nil {
			return fmt.Errorf("apply user state delta: %w", err)
		}
	}
	if len(sessionDelta) > 0 {
		sessionState := sessionStateFromMerged(ls.state)
		maps.Copy(sessionState, sessionDelta)
		sessionStateJSON, err := json.Marshal(sessionState)
		if err != nil {
			return fmt.Errorf("marshal session state: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE adk_sessions SET state = ?, update_time = ? WHERE app_name = ? AND user_id = ? AND session_id = ?`,
			string(sessionStateJSON), event.Timestamp.UnixMicro(), ls.appName, ls.userID, ls.sessionID,
		)
		if err != nil {
			return fmt.Errorf("update session state: %w", err)
		}
	}

	// Insert event.
	var ordinal int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ordinal), -1) + 1 FROM adk_events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		ls.appName, ls.userID, ls.sessionID,
	).Scan(&ordinal); err != nil {
		return fmt.Errorf("allocate event ordinal: %w", err)
	}
	if err := s.insertEvent(ctx, tx, ls, event, ordinal, now); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	// Update session timestamp.
	_, err = tx.ExecContext(ctx,
		`UPDATE adk_sessions SET update_time = ?, revision = revision + 1 WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		event.Timestamp.UnixMicro(), ls.appName, ls.userID, ls.sessionID,
	)
	if err != nil {
		return fmt.Errorf("update session timestamp: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append event: %w", err)
	}

	// Update in-memory session after successful commit.
	ls.events = append(ls.events, trimTempDeltaState(event))
	if len(sessionDelta) > 0 {
		maps.Copy(ls.state, sessionDelta)
	}
	for key, value := range appDelta {
		ls.state[adksession.KeyPrefixApp+key] = value
	}
	for key, value := range userDelta {
		ls.state[adksession.KeyPrefixUser+key] = value
	}
	ls.updatedAt = event.Timestamp
	ls.revision++

	return nil
}

// --- internal helpers ---

func (s *AdkSessionService) upsertAppState(ctx context.Context, tx *sql.Tx, appName string) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO adk_app_state (app_name, state, update_time) VALUES (?, '{}', ?)
		 ON CONFLICT(app_name) DO NOTHING`,
		appName, now.UnixMicro(),
	)
	return err
}

func (s *AdkSessionService) upsertUserState(ctx context.Context, tx *sql.Tx, appName, userID string) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO adk_user_state (app_name, user_id, state, update_time) VALUES (?, ?, '{}', ?)
		 ON CONFLICT(app_name, user_id) DO NOTHING`,
		appName, userID, now.UnixMicro(),
	)
	return err
}

func (s *AdkSessionService) applyStateDelta(ctx context.Context, tx *sql.Tx, table, pkColumn, pkValue string, delta map[string]any, now time.Time) error {
	var currentJSON string
	query := fmt.Sprintf(`SELECT state FROM %s WHERE %s = ?`, table, pkColumn)
	err := tx.QueryRowContext(ctx, query, pkValue).Scan(&currentJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			currentJSON = "{}"
		} else {
			return fmt.Errorf("read %s state: %w", table, err)
		}
	}
	var current map[string]any
	if err := json.Unmarshal([]byte(currentJSON), &current); err != nil {
		return fmt.Errorf("unmarshal %s state: %w", table, err)
	}
	maps.Copy(current, delta)
	merged, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("marshal %s state: %w", table, err)
	}
	upsert := fmt.Sprintf(
		`INSERT INTO %s (%s, state, update_time) VALUES (?, ?, ?)
		 ON CONFLICT(%s) DO UPDATE SET state = excluded.state, update_time = excluded.update_time`,
		table, pkColumn, pkColumn,
	)
	_, err = tx.ExecContext(ctx, upsert, pkValue, string(merged), now.UnixMicro())
	return err
}

func (s *AdkSessionService) applyUserStateDelta(ctx context.Context, tx *sql.Tx, appName, userID string, delta map[string]any, now time.Time) error {
	var currentJSON string
	err := tx.QueryRowContext(ctx,
		`SELECT state FROM adk_user_state WHERE app_name = ? AND user_id = ?`,
		appName, userID,
	).Scan(&currentJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			currentJSON = "{}"
		} else {
			return fmt.Errorf("read user state: %w", err)
		}
	}
	var current map[string]any
	if err := json.Unmarshal([]byte(currentJSON), &current); err != nil {
		return fmt.Errorf("unmarshal user state: %w", err)
	}
	maps.Copy(current, delta)
	merged, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("marshal user state: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO adk_user_state (app_name, user_id, state, update_time) VALUES (?, ?, ?, ?)
		 ON CONFLICT(app_name, user_id) DO UPDATE SET state = excluded.state, update_time = excluded.update_time`,
		appName, userID, string(merged), now.UnixMicro(),
	)
	return err
}

func (s *AdkSessionService) insertEvent(ctx context.Context, tx *sql.Tx, lm *localSession, event *adksession.Event, ordinal int64, now time.Time) error {
	actionsJSON, err := json.Marshal(event.Actions)
	if err != nil {
		return fmt.Errorf("marshal actions: %w", err)
	}

	var contentJSON, groundingJSON, customJSON, usageJSON, citationJSON []byte
	if event.Content != nil {
		contentJSON, err = json.Marshal(event.Content)
		if err != nil {
			return fmt.Errorf("marshal content: %w", err)
		}
	}
	if event.GroundingMetadata != nil {
		groundingJSON, err = json.Marshal(event.GroundingMetadata)
		if err != nil {
			return fmt.Errorf("marshal grounding metadata: %w", err)
		}
	}
	if len(event.CustomMetadata) > 0 {
		customJSON, err = json.Marshal(event.CustomMetadata)
		if err != nil {
			return fmt.Errorf("marshal custom metadata: %w", err)
		}
	}
	if event.UsageMetadata != nil {
		usageJSON, err = json.Marshal(event.UsageMetadata)
		if err != nil {
			return fmt.Errorf("marshal usage metadata: %w", err)
		}
	}
	if event.CitationMetadata != nil {
		citationJSON, err = json.Marshal(event.CitationMetadata)
		if err != nil {
			return fmt.Errorf("marshal citation metadata: %w", err)
		}
	}

	var toolIDsJSON, routesJSON, outputJSON, nodeInfoJSON, requestedInputJSON []byte
	if len(event.LongRunningToolIDs) > 0 {
		toolIDsJSON, err = json.Marshal(event.LongRunningToolIDs)
		if err != nil {
			return fmt.Errorf("marshal long-running tool IDs: %w", err)
		}
	}
	if len(event.Routes) > 0 {
		routesJSON, err = json.Marshal(event.Routes)
		if err != nil {
			return fmt.Errorf("marshal routes: %w", err)
		}
	}
	if event.Output != nil {
		outputJSON, err = json.Marshal(event.Output)
		if err != nil {
			return fmt.Errorf("marshal output: %w", err)
		}
	}
	if event.NodeInfo != nil {
		nodeInfoJSON, err = json.Marshal(event.NodeInfo)
		if err != nil {
			return fmt.Errorf("marshal node info: %w", err)
		}
	}
	if event.RequestedInput != nil {
		requestedInputJSON, err = json.Marshal(event.RequestedInput)
		if err != nil {
			return fmt.Errorf("marshal requested input: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO adk_events (
			id, app_name, user_id, session_id, ordinal,
			invocation_id, author, actions, long_running_tool_ids,
			routes, output, node_info, requested_input,
			branch, isolation_scope, timestamp,
			content, grounding_metadata, custom_metadata, usage_metadata, citation_metadata,
			error_code, error_message, partial, turn_complete, interrupted
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID,
		lm.appName, lm.userID, lm.sessionID,
		ordinal,
		event.InvocationID,
		event.Author,
		string(actionsJSON),
		nullIfEmpty(toolIDsJSON),
		nullIfEmpty(routesJSON),
		nullIfEmpty(outputJSON),
		nullIfEmpty(nodeInfoJSON),
		nullIfEmpty(requestedInputJSON),
		nullString(event.Branch),
		nullString(event.IsolationScope),
		event.Timestamp.UnixMicro(),
		nullIfEmpty(contentJSON),
		nullIfEmpty(groundingJSON),
		nullIfEmpty(customJSON),
		nullIfEmpty(usageJSON),
		nullIfEmpty(citationJSON),
		nullString(event.ErrorCode),
		nullString(event.ErrorMessage),
		boolToInt(event.Partial),
		boolToInt(event.TurnComplete),
		boolToInt(event.Interrupted),
	)
	return err
}

// --- localSession implements adksession.Session ---

type localSession struct {
	appName   string
	userID    string
	sessionID string

	mu        sync.RWMutex
	events    []*adksession.Event
	state     map[string]any
	updatedAt time.Time
	revision  int64
}

func newLocalSession(appName, userID, sessionID string, updatedAt time.Time) *localSession {
	return &localSession{
		appName:   appName,
		userID:    userID,
		sessionID: sessionID,
		state:     make(map[string]any),
		updatedAt: updatedAt,
	}
}

func (s *localSession) ID() string                { return s.sessionID }
func (s *localSession) AppName() string           { return s.appName }
func (s *localSession) UserID() string            { return s.userID }
func (s *localSession) LastUpdateTime() time.Time { return s.updatedAt }

func (s *localSession) State() adksession.State {
	return &sessionState{mu: &s.mu, state: s.state}
}

func (s *localSession) Events() adksession.Events {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sessionEvents(s.events)
}

type sessionState struct {
	mu    *sync.RWMutex
	state map[string]any
}

func (s *sessionState) Get(key string) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.state[key]
	if !ok {
		return nil, adksession.ErrStateKeyNotExist
	}
	return val, nil
}

func (s *sessionState) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[key] = value
	return nil
}

func (s *sessionState) All() iter.Seq2[string, any] {
	s.mu.RLock()
	copied := maps.Clone(s.state)
	s.mu.RUnlock()
	return func(yield func(string, any) bool) {
		for k, v := range copied {
			if !yield(k, v) {
				return
			}
		}
	}
}

type sessionEvents []*adksession.Event

func (e sessionEvents) All() iter.Seq[*adksession.Event] {
	return func(yield func(*adksession.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}

func (e sessionEvents) Len() int { return len(e) }
func (e sessionEvents) At(i int) *adksession.Event {
	if i >= 0 && i < len(e) {
		return e[i]
	}
	return nil
}

var (
	_ adksession.Session = (*localSession)(nil)
	_ adksession.State   = (*sessionState)(nil)
	_ adksession.Events  = (sessionEvents)(nil)
)

// --- utility functions ---

func extractStateDeltas(delta map[string]any) (app, user, session map[string]any) {
	app = make(map[string]any)
	user = make(map[string]any)
	session = make(map[string]any)
	if delta == nil {
		return
	}
	for key, value := range delta {
		if after, found := strings.CutPrefix(key, adksession.KeyPrefixApp); found {
			app[after] = value
		} else if after, found := strings.CutPrefix(key, adksession.KeyPrefixUser); found {
			user[after] = value
		} else if !strings.HasPrefix(key, adksession.KeyPrefixTemp) {
			session[key] = value
		}
	}
	return
}

func mergeStates(app, user, current map[string]any) map[string]any {
	merged := make(map[string]any, len(app)+len(user)+len(current))
	maps.Copy(merged, current)
	for key, value := range app {
		merged[adksession.KeyPrefixApp+key] = value
	}
	for key, value := range user {
		merged[adksession.KeyPrefixUser+key] = value
	}
	return merged
}

func sessionStateFromMerged(state map[string]any) map[string]any {
	_, _, current := extractStateDeltas(state)
	return current
}

func trimTempDeltaState(event *adksession.Event) *adksession.Event {
	if len(event.Actions.StateDelta) == 0 {
		return event
	}
	filtered := make(map[string]any)
	for key, value := range event.Actions.StateDelta {
		if !strings.HasPrefix(key, adksession.KeyPrefixTemp) {
			filtered[key] = value
		}
	}
	event.Actions.StateDelta = filtered
	return event
}

func scanEvents(rows *sql.Rows, after time.Time) ([]*adksession.Event, error) {
	var loaded []*adksession.Event
	for rows.Next() {
		var (
			id, invocationID, author, actionsJSON                                 string
			toolIDsJSON, routesJSON, outputJSON, nodeInfoJSON, requestedInputJSON []byte
			branch, isolationScope                                                *string
			timestamp                                                             int64
			contentJSON, groundingJSON, customJSON, usageJSON, citationJSON       []byte
			errorCode, errorMessage                                               *string
			partialInt, turnCompleteInt, interruptedInt                           int
		)
		if err := rows.Scan(
			&id, &invocationID, &author, &actionsJSON,
			&toolIDsJSON, &routesJSON, &outputJSON, &nodeInfoJSON, &requestedInputJSON,
			&branch, &isolationScope, &timestamp,
			&contentJSON, &groundingJSON, &customJSON, &usageJSON, &citationJSON,
			&errorCode, &errorMessage, &partialInt, &turnCompleteInt, &interruptedInt,
		); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}

		if !after.IsZero() && time.UnixMicro(timestamp).Before(after) {
			break // Events are DESC by timestamp; remaining are older.
		}

		event := &adksession.Event{
			ID:           id,
			InvocationID: invocationID,
			Author:       author,
			Timestamp:    time.UnixMicro(timestamp),
			Actions:      adksession.EventActions{StateDelta: make(map[string]any), ArtifactDelta: make(map[string]int64)},
		}

		if len(actionsJSON) > 0 {
			if err := json.Unmarshal([]byte(actionsJSON), &event.Actions); err != nil {
				return nil, fmt.Errorf("unmarshal actions for event %s: %w", id, err)
			}
		}
		if len(toolIDsJSON) > 0 {
			if err := json.Unmarshal(toolIDsJSON, &event.LongRunningToolIDs); err != nil {
				return nil, fmt.Errorf("unmarshal long-running tool IDs for event %s: %w", id, err)
			}
		}
		if len(routesJSON) > 0 {
			if err := json.Unmarshal(routesJSON, &event.Routes); err != nil {
				return nil, fmt.Errorf("unmarshal routes for event %s: %w", id, err)
			}
		}
		if len(outputJSON) > 0 {
			if err := json.Unmarshal(outputJSON, &event.Output); err != nil {
				return nil, fmt.Errorf("unmarshal output for event %s: %w", id, err)
			}
		}
		if len(nodeInfoJSON) > 0 {
			if err := json.Unmarshal(nodeInfoJSON, &event.NodeInfo); err != nil {
				return nil, fmt.Errorf("unmarshal node info for event %s: %w", id, err)
			}
		}
		if len(requestedInputJSON) > 0 {
			if err := json.Unmarshal(requestedInputJSON, &event.RequestedInput); err != nil {
				return nil, fmt.Errorf("unmarshal requested input for event %s: %w", id, err)
			}
		}
		if branch != nil {
			event.Branch = *branch
		}
		if isolationScope != nil {
			event.IsolationScope = *isolationScope
		}
		if len(contentJSON) > 0 {
			var content genai.Content
			if err := json.Unmarshal(contentJSON, &content); err != nil {
				return nil, fmt.Errorf("unmarshal content for event %s: %w", id, err)
			}
			event.Content = &content
		}
		if len(groundingJSON) > 0 {
			var gm genai.GroundingMetadata
			if err := json.Unmarshal(groundingJSON, &gm); err != nil {
				return nil, fmt.Errorf("unmarshal grounding metadata for event %s: %w", id, err)
			}
			event.GroundingMetadata = &gm
		}
		if len(customJSON) > 0 {
			if err := json.Unmarshal(customJSON, &event.CustomMetadata); err != nil {
				return nil, fmt.Errorf("unmarshal custom metadata for event %s: %w", id, err)
			}
		}
		if len(usageJSON) > 0 {
			var um genai.GenerateContentResponseUsageMetadata
			if err := json.Unmarshal(usageJSON, &um); err != nil {
				return nil, fmt.Errorf("unmarshal usage metadata for event %s: %w", id, err)
			}
			event.UsageMetadata = &um
		}
		if len(citationJSON) > 0 {
			var cm genai.CitationMetadata
			if err := json.Unmarshal(citationJSON, &cm); err != nil {
				return nil, fmt.Errorf("unmarshal citation metadata for event %s: %w", id, err)
			}
			event.CitationMetadata = &cm
		}
		if errorCode != nil {
			event.ErrorCode = *errorCode
		}
		if errorMessage != nil {
			event.ErrorMessage = *errorMessage
		}
		event.Partial = partialInt != 0
		event.TurnComplete = turnCompleteInt != 0
		event.Interrupted = interruptedInt != 0

		// Reconstruct LLMResponse fields
		event.LLMResponse = model.LLMResponse{
			Content:           event.Content,
			GroundingMetadata: event.GroundingMetadata,
			CustomMetadata:    event.CustomMetadata,
			UsageMetadata:     event.UsageMetadata,
			CitationMetadata:  event.CitationMetadata,
			ErrorCode:         event.ErrorCode,
			ErrorMessage:      event.ErrorMessage,
			Partial:           event.Partial,
			TurnComplete:      event.TurnComplete,
			Interrupted:       event.Interrupted,
		}

		loaded = append(loaded, event)
	}
	return loaded, nil
}

func reverseEvents(events []*adksession.Event) {
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
}

func nullIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type stateReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *AdkSessionService) appState(ctx context.Context, reader stateReader, appName string) (map[string]any, error) {
	var encoded string
	if err := reader.QueryRowContext(ctx, `SELECT state FROM adk_app_state WHERE app_name = ?`, appName).Scan(&encoded); err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *AdkSessionService) userState(ctx context.Context, reader stateReader, appName, userID string) (map[string]any, error) {
	var encoded string
	if err := reader.QueryRowContext(ctx, `SELECT state FROM adk_user_state WHERE app_name = ? AND user_id = ?`, appName, userID).Scan(&encoded); err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return nil, err
	}
	return state, nil
}
