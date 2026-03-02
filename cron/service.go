package cron

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PlatoX-Type/monet-bot/bus"
	"github.com/PlatoX-Type/monet-bot/config"
	"github.com/robfig/cron/v3"
)

// Job represents a scheduled job (static from config or dynamic from the LLM).
type Job struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"` // "cron", "every", "at"
	CronExpr     string `json:"cron_expr,omitempty"`
	EverySeconds int    `json:"every_seconds,omitempty"`
	AtUnix       int64  `json:"at_unix,omitempty"`
	Tz           string `json:"tz,omitempty"`
	Message      string `json:"message"`
	Channel      string `json:"channel"`
	ChatID       string `json:"chat_id"`
	DeleteAfter  bool   `json:"delete_after,omitempty"`
	EntryID      cron.EntryID
}

// Service manages scheduled jobs using robfig/cron.
type Service struct {
	bus       *bus.MessageBus
	cron      *cron.Cron
	mu        sync.Mutex
	jobs      map[string]*Job
	nextID    int
	storePath string
}

// New creates a cron service. Static jobs from config are added immediately.
func New(mb *bus.MessageBus, workspace string, staticJobs []config.CronJobConfig) *Service {
	s := &Service{
		bus:       mb,
		cron:      cron.New(cron.WithSeconds()),
		jobs:      make(map[string]*Job),
		storePath: filepath.Join(workspace, "cron_jobs.json"),
	}

	// Load persisted dynamic jobs
	s.loadFromDisk()

	// Add static jobs from config
	for _, j := range staticJobs {
		s.addInternal(&Job{
			ID:       fmt.Sprintf("static-%s", j.Name),
			Name:     j.Name,
			Kind:     "cron",
			CronExpr: j.Schedule,
			Message:  j.Task,
			Channel:  "cron",
			ChatID:   "cron-" + j.Name,
		})
	}

	return s
}

// Run starts the cron scheduler and blocks.
func (s *Service) Run() {
	s.cron.Start()
	log.Println("[cron] service started")
	select {} // block forever
}

// AddJob adds a dynamic job at runtime (called by the cron tool).
func (s *Service) AddJob(name, kind, cronExpr, tz, message, channel, chatID string, everySeconds int, atUnix int64, deleteAfter bool) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	job := &Job{
		ID:           fmt.Sprintf("dyn-%d", s.nextID),
		Name:         name,
		Kind:         kind,
		CronExpr:     cronExpr,
		EverySeconds: everySeconds,
		AtUnix:       atUnix,
		Tz:           tz,
		Message:      message,
		Channel:      channel,
		ChatID:       chatID,
		DeleteAfter:  deleteAfter,
	}

	s.addInternal(job)
	s.saveToDisk()
	return job
}

// ListJobs returns all active jobs.
func (s *Service) ListJobs() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	jobs := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	return jobs
}

// RemoveJob removes a job by ID. Returns true if found.
func (s *Service) RemoveJob(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return false
	}

	s.cron.Remove(job.EntryID)
	delete(s.jobs, id)
	s.saveToDisk()
	return true
}

func (s *Service) addInternal(job *Job) {
	var entryID cron.EntryID
	var err error

	fire := func() {
		log.Printf("[cron] firing job: %s (%s)", job.Name, job.ID)
		s.bus.Inbound <- bus.InboundMessage{
			Channel:   job.Channel,
			ChatID:    job.ChatID,
			User:      "cron",
			Text:      job.Message,
			Timestamp: time.Now(),
		}
		if job.DeleteAfter {
			// Self-remove after firing
			go func(id string) {
				s.mu.Lock()
				defer s.mu.Unlock()
				if j, ok := s.jobs[id]; ok {
					s.cron.Remove(j.EntryID)
					delete(s.jobs, id)
					s.saveToDisk()
					log.Printf("[cron] one-time job removed: %s", id)
				}
			}(job.ID)
		}
	}

	switch job.Kind {
	case "cron":
		spec := job.CronExpr
		if job.Tz != "" {
			spec = fmt.Sprintf("CRON_TZ=%s %s", job.Tz, spec)
		}
		// robfig/cron with seconds expects 6 fields; if user provides 5-field standard cron, wrap it
		entryID, err = s.cron.AddFunc(spec, fire)
		if err != nil {
			// Try as standard 5-field by prepending "0 " (run at second 0)
			entryID, err = s.cron.AddFunc("0 "+spec, fire)
		}
	case "every":
		spec := fmt.Sprintf("@every %ds", job.EverySeconds)
		entryID, err = s.cron.AddFunc(spec, fire)
	case "at":
		// One-time execution: schedule a goroutine that waits
		delay := time.Until(time.Unix(job.AtUnix, 0))
		if delay <= 0 {
			// Already past — fire immediately
			go fire()
			entryID = 0
		} else {
			go func() {
				time.Sleep(delay)
				fire()
			}()
			entryID = 0
		}
	default:
		log.Printf("[cron] unknown job kind: %s", job.Kind)
		return
	}

	if err != nil {
		log.Printf("[cron] error scheduling '%s': %v", job.Name, err)
		return
	}

	job.EntryID = entryID
	s.jobs[job.ID] = job
	log.Printf("[cron] scheduled: %s (%s, %s)", job.Name, job.ID, job.Kind)
}

// saveToDisk persists dynamic jobs to JSON (excludes static jobs).
func (s *Service) saveToDisk() {
	var dynamicJobs []*Job
	for _, j := range s.jobs {
		if j.ID[:4] != "stat" { // skip "static-*"
			dynamicJobs = append(dynamicJobs, j)
		}
	}
	data, _ := json.MarshalIndent(dynamicJobs, "", "  ")
	os.WriteFile(s.storePath, data, 0o644)
}

// loadFromDisk restores dynamic jobs.
func (s *Service) loadFromDisk() {
	data, err := os.ReadFile(s.storePath)
	if err != nil {
		return
	}
	var jobs []*Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return
	}
	for _, j := range jobs {
		// Track nextID so new IDs don't collide
		var n int
		if _, err := fmt.Sscanf(j.ID, "dyn-%d", &n); err == nil && n >= s.nextID {
			s.nextID = n
		}
		s.addInternal(j)
	}
	if len(jobs) > 0 {
		log.Printf("[cron] restored %d dynamic jobs from disk", len(jobs))
	}
}
