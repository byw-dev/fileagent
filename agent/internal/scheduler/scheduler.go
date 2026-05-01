// Package scheduler manages timed and continuous file collection rules.
// It wraps robfig/cron for cron-triggered rules and resolves time variable
// placeholders in path templates.
package scheduler

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// CollectionRule describes a single file collection policy received from the
// Control Plane.
type CollectionRule struct {
	// RuleID is the unique identifier of the rule.
	RuleID string
	// Name is a human-readable label.
	Name string
	// Mode is the collection mode: "watch" or "cron".
	Mode string
	// SourcePathTemplate is the directory to monitor, possibly containing time variables.
	SourcePathTemplate string
	// FileGlob is the glob pattern matched against file base names.
	FileGlob string
	// UploadBucket is the MinIO bucket name to upload files into.
	UploadBucket string
	// UploadPathTemplate is the object key prefix template for uploaded files.
	UploadPathTemplate string
	// WatchRecursive enables recursive subdirectory monitoring.
	WatchRecursive bool
	// WatchSubdirPattern is an optional glob for subdirectory filtering.
	WatchSubdirPattern string
	// CronExpr is the cron expression used when Mode="cron".
	CronExpr string
	// RunOnceOnStart causes the callback to fire immediately when the rule is added.
	RunOnceOnStart bool
	// AppendMode controls whether files are appended or replaced.
	AppendMode string
	// Enabled controls whether this rule is active.
	Enabled bool
}

// entry tracks an active cron job for a rule.
type entry struct {
	entryID cron.EntryID
	rule    CollectionRule
}

// Scheduler manages a set of CollectionRules, triggering callbacks on schedule.
type Scheduler struct {
	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]*entry
	logger  *zap.Logger
}

// New constructs a Scheduler using a UTC-based cron runner.
func New(logger *zap.Logger) *Scheduler {
	return &Scheduler{
		cron:    cron.New(cron.WithLocation(time.UTC)),
		entries: make(map[string]*entry),
		logger:  logger,
	}
}

// AddRule registers a CollectionRule with the scheduler. The callback is
// invoked with the resolved source path each time the rule fires. If
// RunOnceOnStart is true, the callback is also invoked immediately.
// Returns an error if the cron expression is invalid.
func (s *Scheduler) AddRule(rule CollectionRule, callback func(resolvedPath string)) error {
	if !rule.Enabled {
		return nil
	}

	resolved := ResolvePath(rule.SourcePathTemplate, time.Now().UTC())

	if rule.RunOnceOnStart {
		callback(resolved)
	}

	if rule.CronExpr == "" {
		// Watch-mode rules don't need a cron schedule.
		s.mu.Lock()
		s.entries[rule.RuleID] = &entry{rule: rule}
		s.mu.Unlock()
		return nil
	}

	eid, err := s.cron.AddFunc(rule.CronExpr, func() {
		path := ResolvePath(rule.SourcePathTemplate, time.Now().UTC())
		s.logger.Info("scheduler: rule fired", zap.String("rule_id", rule.RuleID), zap.String("path", path))
		callback(path)
	})
	if err != nil {
		return fmt.Errorf("scheduler: add rule %q: invalid cron expr %q: %w", rule.RuleID, rule.CronExpr, err)
	}

	s.mu.Lock()
	s.entries[rule.RuleID] = &entry{entryID: eid, rule: rule}
	s.mu.Unlock()

	s.logger.Info("scheduler: rule added", zap.String("rule_id", rule.RuleID), zap.String("cron", rule.CronExpr))
	return nil
}

// RemoveRule removes the rule identified by ruleID from the scheduler. If the
// rule is not found, the call is a no-op.
func (s *Scheduler) RemoveRule(ruleID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[ruleID]
	if !ok {
		return
	}
	if e.entryID != 0 {
		s.cron.Remove(e.entryID)
	}
	delete(s.entries, ruleID)
	s.logger.Info("scheduler: rule removed", zap.String("rule_id", ruleID))
}

// Start begins the cron scheduler. It is safe to call Start before or after
// adding rules.
func (s *Scheduler) Start() {
	s.cron.Start()
}

// Stop gracefully shuts down the cron scheduler, waiting for any running jobs
// to complete.
func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
}

// ResolvePath substitutes time variables in template with the given UTC time
// and returns the resolved string.
//
// Supported variables:
//
//	{yyyy} → 4-digit year
//	{yy}   → 2-digit year
//	{mm}   → 2-digit month (01-12)
//	{dd}   → 2-digit day (01-31)
//	{HH}   → 2-digit hour (00-23)
//	{MM}   → 2-digit minute (00-59)
func ResolvePath(template string, t time.Time) string {
	r := strings.NewReplacer(
		"{yyyy}", fmt.Sprintf("%04d", t.Year()),
		"{yy}", fmt.Sprintf("%02d", t.Year()%100),
		"{mm}", fmt.Sprintf("%02d", int(t.Month())),
		"{dd}", fmt.Sprintf("%02d", t.Day()),
		"{HH}", fmt.Sprintf("%02d", t.Hour()),
		"{MM}", fmt.Sprintf("%02d", t.Minute()),
	)
	return r.Replace(template)
}
