package transport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const (
	diagnosticJobProtocol  = "config-job-v1"
	diagnosticJobNamespace = "diagnostic_jobs_v1"
	diagnosticPreparedTTL  = 2 * time.Minute
	diagnosticHistoryTTL   = 30 * time.Minute
	diagnosticRecordTTL    = 24 * time.Hour
	diagnosticMaxHistory   = 32
	diagnosticKVTimeout    = 3 * time.Second
)

type diagnosticCommand struct {
	Protocol string          `json:"protocol"`
	Action   string          `json:"action"`
	OwnerID  string          `json:"owner_id"`
	TaskID   string          `json:"task_id"`
	Config   json.RawMessage `json:"config,omitempty"`
	Receipt  string          `json:"receipt,omitempty"`
	Seal     string          `json:"seal,omitempty"`
	IssuedAt int64           `json:"issued_at,omitempty"`
	replay   bool
}

type diagnosticJob struct {
	TaskID      string                       `json:"task_id"`
	OwnerID     string                       `json:"owner_id"`
	State       string                       `json:"state"`
	Receipt     string                       `json:"receipt,omitempty"`
	CreatedAt   time.Time                    `json:"created_at"`
	ExpiresAt   time.Time                    `json:"expires_at"`
	UpdatedAt   time.Time                    `json:"updated_at"`
	Result      *pluginv1.TestConfigResponse `json:"result,omitempty"`
	Error       string                       `json:"error,omitempty"`
	Snapshot    protocol.Config              `json:"snapshot"`
	Fingerprint string                       `json:"fingerprint"`
}

type diagnosticJobStore struct {
	mu           sync.Mutex
	ownerID      string
	sealKey      string
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	active       string
	scopedActive int
	jobs         map[string]*diagnosticJob
	workers      sync.WaitGroup
	storageError string
}

func diagnosticRandomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func newDiagnosticJobStore() *diagnosticJobStore {
	ctx, cancel := context.WithCancel(context.Background())
	return &diagnosticJobStore{ownerID: diagnosticRandomID(), sealKey: diagnosticRandomID(), ctx: ctx, cancel: cancel, jobs: make(map[string]*diagnosticJob)}
}

func (s *diagnosticJobStore) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
}

func validDiagnosticID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}

// UI submits only diagnostic_task. Persisted envelopes also contain the
// normalized production config. Foreign-boot envelopes restore that config
// but discard the command; a foreign-boot command-only submission is rejected.
func (t *Transport) parseDiagnosticEnvelope(raw []byte) ([]byte, *diagnosticCommand, bool, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, nil, false, nil
	}
	commandRaw, exists := object["diagnostic_task"]
	if !exists {
		return nil, nil, false, nil
	}
	onlyCommand := len(object) == 1
	var command diagnosticCommand
	decoder := json.NewDecoder(bytes.NewReader(commandRaw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return nil, nil, true, fmt.Errorf("invalid diagnostic task command")
	}
	if decoder.Decode(new(any)) != io.EOF || command.Protocol != diagnosticJobProtocol ||
		!validDiagnosticID(command.TaskID) || !validDiagnosticID(command.OwnerID) ||
		(command.Action != "prepare" && command.Action != "commit") {
		return nil, nil, true, fmt.Errorf("invalid diagnostic task protocol or identity")
	}
	delete(object, "diagnostic_task")
	clean, _ := json.Marshal(object)
	s := t.diagnosticJobs
	if s == nil || s.ownerID == "" || s.sealKey == "" {
		return nil, nil, true, fmt.Errorf("diagnostic task service unavailable")
	}
	if command.OwnerID != s.ownerID {
		if onlyCommand {
			return nil, nil, true, fmt.Errorf("diagnostic runtime changed; reopen the configuration page")
		}
		cfg, err := protocol.ParseConfig(clean)
		if err != nil {
			return nil, nil, true, err
		}
		return protocol.JSONBytes(cfg), nil, true, nil
	}
	if !onlyCommand {
		if cfg, err := protocol.ParseConfig(clean); err != nil {
			return nil, nil, true, err
		} else {
			clean = protocol.JSONBytes(cfg)
		}
	}
	if command.Action == "prepare" {
		if command.Receipt != "" || len(command.Config) == 0 {
			return nil, nil, true, fmt.Errorf("prepare requires a diagnostic snapshot and no receipt")
		}
		cfg, err := protocol.ParseConfig(command.Config)
		if err != nil {
			return nil, nil, true, err
		}
		if !cfg.DegradationCheck || (cfg.DegradationCheckAccountID <= 0 && len(cfg.DegradationCheckAccountIDs) == 0) {
			return nil, nil, true, fmt.Errorf("diagnostic task requires an explicit account snapshot")
		}
		cfg.BPSReenabledAccounts = nil
		command.Config = protocol.JSONBytes(cfg)
	} else if len(command.Config) != 0 || !validDiagnosticID(command.Receipt) {
		return nil, nil, true, fmt.Errorf("commit requires the prepared receipt and no replacement snapshot")
	}
	if onlyCommand {
		if command.Seal != "" || command.IssuedAt != 0 {
			return nil, nil, true, fmt.Errorf("new diagnostic commands cannot include an envelope seal")
		}
	} else {
		if !hmac.Equal([]byte(command.Seal), []byte(s.envelopeSeal(clean, command))) {
			return nil, nil, true, fmt.Errorf("diagnostic configuration envelope is not valid for this runtime")
		}
		command.replay = true
	}
	return clean, &command, true, nil
}

func (s *diagnosticJobStore) envelopeSeal(base []byte, command diagnosticCommand) string {
	command.Seal = ""
	mac := hmac.New(sha256.New, []byte(s.sealKey))
	_, _ = mac.Write(base)
	_, _ = mac.Write(protocol.JSONBytes(command))
	return hex.EncodeToString(mac.Sum(nil))
}

func (t *Transport) normalizeDiagnosticCommand(raw []byte) ([]byte, bool, error) {
	clean, command, handled, err := t.parseDiagnosticEnvelope(raw)
	if !handled || err != nil || command == nil {
		return clean, handled, err
	}
	s := t.diagnosticJobs
	if command.replay {
		var object map[string]json.RawMessage
		_ = json.Unmarshal(clean, &object)
		object["diagnostic_task"] = protocol.JSONBytes(command)
		normalized, err := json.Marshal(object)
		return normalized, true, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, true, errTransportStopped
	}
	if command.Action == "commit" {
		job := s.jobs[command.TaskID]
		if job == nil || subtle.ConstantTimeCompare([]byte(job.Receipt), []byte(command.Receipt)) != 1 {
			s.mu.Unlock()
			return nil, true, fmt.Errorf("diagnostic task or prepared receipt does not match")
		}
		if job.State == "expired" || (job.State == "prepared" && time.Now().After(job.ExpiresAt)) {
			s.mu.Unlock()
			return nil, true, fmt.Errorf("diagnostic task expired; create a new task")
		}
	}
	if s.jobs[command.TaskID] == nil && s.active != "" && s.active != command.TaskID {
		active := s.jobs[s.active]
		if s.active == autoDegradationDiagnosticID || (active != nil && !(active.State == "prepared" && time.Now().After(active.ExpiresAt))) {
			s.mu.Unlock()
			return nil, true, fmt.Errorf("another diagnostic task is busy; wait for it to finish")
		}
	}
	s.mu.Unlock()
	t.mu.RLock()
	cfg, closed := t.cfg.Clone(), t.closed
	t.mu.RUnlock()
	if closed {
		return nil, true, errTransportStopped
	}
	var object map[string]json.RawMessage
	base := protocol.JSONBytes(cfg)
	_ = json.Unmarshal(base, &object)
	command.IssuedAt = time.Now().UnixMilli()
	command.Seal = s.envelopeSeal(base, *command)
	object["diagnostic_task"] = protocol.JSONBytes(command)
	normalized, err := json.Marshal(object)
	return normalized, true, err
}

func diagnosticJobKey(owner, id string) string {
	digest := sha256.Sum256([]byte(owner + "/" + id))
	return "job-" + hex.EncodeToString(digest[:])
}

func (s *diagnosticJobStore) pruneLocked(now time.Time) {
	for id, job := range s.jobs {
		if job.State == "prepared" && now.After(job.ExpiresAt) {
			job.State, job.UpdatedAt, job.Error = "expired", now, "prepared task expired without an explicit commit"
			if s.active == id {
				s.active = ""
			}
		}
		if id != s.active && now.Sub(job.UpdatedAt) > diagnosticHistoryTTL {
			delete(s.jobs, id)
		}
	}
	for len(s.jobs) >= diagnosticMaxHistory {
		var oldest string
		for id, job := range s.jobs {
			if id != s.active && (oldest == "" || job.UpdatedAt.Before(s.jobs[oldest].UpdatedAt)) {
				oldest = id
			}
		}
		if oldest == "" {
			break
		}
		delete(s.jobs, oldest)
	}
}

func diagnosticWrite(ctx context.Context, host pluginv1.HostServiceClient, job *diagnosticJob) error {
	if host == nil {
		return fmt.Errorf("diagnostic host storage unavailable")
	}
	writeCtx, cancel := context.WithTimeout(ctx, diagnosticKVTimeout)
	defer cancel()
	_, err := host.KVSet(writeCtx, &pluginv1.KVSetRequest{Namespace: diagnosticJobNamespace,
		Key: diagnosticJobKey(job.OwnerID, job.TaskID), Value: protocol.JSONBytes(job), TtlSeconds: int64(diagnosticRecordTTL / time.Second)})
	if err != nil {
		return fmt.Errorf("diagnostic task storage write failed; no automatic retry")
	}
	return nil
}

func (t *Transport) applyDiagnosticCommand(ctx context.Context, command *diagnosticCommand) error {
	t.mu.RLock()
	host, closed := t.host, t.closed
	t.mu.RUnlock()
	if closed {
		return errTransportStopped
	}
	if host == nil {
		return fmt.Errorf("diagnostic host storage unavailable")
	}
	s := t.diagnosticJobs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil {
		return fmt.Errorf("diagnostic command canceled or plugin stopped")
	}
	now := time.Now()
	s.pruneLocked(now)
	job := s.jobs[command.TaskID]
	if command.Action == "prepare" {
		cfg, err := protocol.ParseConfig(command.Config)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(protocol.JSONBytes(cfg))
		fingerprint := hex.EncodeToString(digest[:])
		if job != nil {
			if job.Fingerprint != fingerprint {
				return fmt.Errorf("diagnostic task ID already binds a different snapshot")
			}
			return nil
		}
		// Saved prepares must never revive after history eviction or expiry.
		if command.replay && (command.IssuedAt <= 0 || now.Sub(time.UnixMilli(command.IssuedAt)) > diagnosticPreparedTTL) {
			return nil
		}
		// A task pruned from memory must not be silently recreated. This also
		// checks that the host implements readable KV before accepting the job.
		readCtx, cancel := context.WithTimeout(ctx, diagnosticKVTimeout)
		record, err := host.KVGet(readCtx, &pluginv1.KVGetRequest{Namespace: diagnosticJobNamespace, Key: diagnosticJobKey(s.ownerID, command.TaskID)})
		cancel()
		if err != nil || record == nil {
			s.storageError = "diagnostic task storage unavailable"
			return fmt.Errorf("diagnostic task storage unavailable")
		}
		if record.Found {
			var previous diagnosticJob
			if json.Unmarshal(record.Value, &previous) != nil || previous.OwnerID != s.ownerID || previous.TaskID != command.TaskID || previous.Fingerprint != fingerprint || !validDiagnosticID(previous.Receipt) {
				s.storageError = "diagnostic task has an unrecognized persisted state; no request was sent"
				return fmt.Errorf("diagnostic task has an unrecognized persisted state; no request was sent")
			}
			switch previous.State {
			case "prepared", "queued", "running", "completed", "failed", "unknown", "expired":
			default:
				s.storageError = "diagnostic task has an unrecognized persisted state; no request was sent"
				return fmt.Errorf("diagnostic task has an unrecognized persisted state; no request was sent")
			}
			if command.replay {
				return nil
			}
			return fmt.Errorf("diagnostic task ID has already been used; its result is no longer retained")
		}
		if s.active != "" && s.active != command.TaskID {
			return fmt.Errorf("another diagnostic task is busy; wait for it to finish")
		}
		receipt := diagnosticRandomID()
		if receipt == "" {
			return fmt.Errorf("cannot create diagnostic receipt")
		}
		job = &diagnosticJob{TaskID: command.TaskID, OwnerID: s.ownerID, State: "prepared", Receipt: receipt,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(diagnosticPreparedTTL), Snapshot: cfg.Clone(), Fingerprint: fingerprint}
		if err := diagnosticWrite(ctx, host, job); err != nil {
			s.storageError = err.Error()
			return err
		}
		s.storageError = ""
		s.jobs[job.TaskID], s.active = job, job.TaskID
		return nil
	}
	if job == nil && command.replay {
		return nil
	}
	if job == nil || subtle.ConstantTimeCompare([]byte(job.Receipt), []byte(command.Receipt)) != 1 {
		return fmt.Errorf("diagnostic task or prepared receipt does not match")
	}
	if job.State != "prepared" {
		if job.State == "expired" {
			if command.replay {
				return nil
			}
			return fmt.Errorf("diagnostic task expired; create a new task")
		}
		return nil // Re-Apply, rollback and client retries never resend it.
	}
	job.State, job.UpdatedAt = "queued", now
	if err := diagnosticWrite(ctx, host, job); err != nil {
		job.State, job.Error = "failed", err.Error()
		s.active, s.storageError = "", err.Error()
		return err
	}
	s.storageError = ""
	s.workers.Add(1)
	go t.runDiagnosticJob(job, host)
	return nil
}

func (t *Transport) runDiagnosticJob(job *diagnosticJob, host pluginv1.HostServiceClient) {
	s := t.diagnosticJobs
	defer s.workers.Done()
	s.mu.Lock()
	if s.closed {
		job.State, job.Error, job.UpdatedAt = "unknown", "plugin stopped before task execution was confirmed", time.Now()
		s.active = ""
		s.mu.Unlock()
		return
	}
	job.State, job.UpdatedAt = "running", time.Now()
	// Persist consumption before resolving credentials or sending anything.
	// No boot resumes this marker, even if the response was lost after sending.
	if err := diagnosticWrite(s.ctx, host, job); err != nil {
		job.State, job.Error, job.UpdatedAt = "failed", err.Error(), time.Now()
		s.active, s.storageError = "", err.Error()
		s.mu.Unlock()
		return
	}
	cfg := job.Snapshot.Clone()
	s.mu.Unlock()
	t.mu.RLock()
	cfg.BPSReenabledAccounts = t.cfg.Clone().BPSReenabledAccounts
	currentHost, closed := t.host, t.closed
	t.mu.RUnlock()
	started := time.Now()
	ctx, cancel := context.WithCancel(s.ctx)
	var response *pluginv1.TestConfigResponse
	if closed || currentHost != host {
		response = &pluginv1.TestConfigResponse{Success: false, Message: "plugin runtime changed before diagnostic execution"}
	} else {
		check, err := t.runBackgroundDegradationCheck(ctx, cfg)
		check.RequestID = job.TaskID
		message := "account degradation check completed"
		if err != nil {
			message = safeError(err)
		}
		status := t.bpsAccountStatusJSON(healthStatusJSON(cfg, nil), cfg)
		response = &pluginv1.TestConfigResponse{Success: err == nil && check.Completed, Message: message,
			LatencyMs: time.Since(started).Milliseconds(), StatusJson: mergeDegradationStatus(status, check)}
	}
	canceled := ctx.Err() != nil
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	job.Result, job.UpdatedAt, job.State = response, time.Now(), "completed"
	if !response.Success {
		job.State = "failed"
	}
	if canceled || s.closed {
		job.State = "unknown"
		job.Error = "diagnostic execution was interrupted; do not retry automatically"
	}
	// Failure leaves the consumed marker durable and never permits resending.
	if !s.closed {
		if err := diagnosticWrite(s.ctx, host, job); err != nil {
			job.Error = "diagnostic result could not be persisted; do not retry automatically"
			s.storageError = job.Error
		}
	}
	s.active = ""
}

func (t *Transport) diagnosticStatusJSON(statusJSON string) string {
	var status map[string]any
	if json.Unmarshal([]byte(statusJSON), &status) != nil {
		status = make(map[string]any)
	}
	t.mu.RLock()
	host, closed := t.host, t.closed
	t.mu.RUnlock()
	s := t.diagnosticJobs
	if s == nil {
		return statusJSON
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	tasks := make([]map[string]any, 0, len(s.jobs))
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		job := s.jobs[id]
		if id != s.active && now.Sub(job.UpdatedAt) > diagnosticHistoryTTL {
			continue
		}
		state := job.State
		if state == "prepared" && now.After(job.ExpiresAt) {
			state = "expired"
		}
		executionBudget, _ := backgroundDegradationBudgets(job.Snapshot)
		item := map[string]any{"task_id": id, "state": state, "execution_timeout_ms": executionBudget.Milliseconds()}
		if state == "prepared" {
			item["receipt"] = job.Receipt
		}
		if job.Result != nil {
			item["result"] = job.Result
		}
		if job.Error != "" {
			item["error"] = job.Error
		}
		tasks = append(tasks, item)
	}
	available := !closed && !s.closed && host != nil && s.ownerID != ""
	summary := map[string]any{"protocol": diagnosticJobProtocol, "owner_id": s.ownerID, "available": available, "tasks": tasks}
	if !available {
		summary["error"] = "diagnostic tasks require a running plugin with host storage"
	} else if s.storageError != "" {
		summary["error"] = s.storageError
	}
	status["diagnostic_tasks"] = summary
	raw, _ := json.Marshal(status)
	return string(raw)
}
