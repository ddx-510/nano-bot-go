package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PlatoX-Type/monet-bot/cron"
)

// CronTool lets the LLM schedule reminders and recurring tasks at runtime.
type CronTool struct {
	Service *cron.Service
	// Current context — set per request by the agent loop.
	Channel string
	ChatID  string
}

func (t *CronTool) Name() string { return "cron" }
func (t *CronTool) Description() string {
	return "Schedule reminders and recurring tasks. Actions: add, list, remove."
}
func (t *CronTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["add", "list", "remove"],
				"description": "Action to perform"
			},
			"message": {
				"type": "string",
				"description": "Reminder message (for add)"
			},
			"every_seconds": {
				"type": "integer",
				"description": "Interval in seconds for recurring tasks"
			},
			"cron_expr": {
				"type": "string",
				"description": "Cron expression like '0 9 * * MON-FRI' (for scheduled tasks)"
			},
			"tz": {
				"type": "string",
				"description": "IANA timezone for cron expressions (e.g. 'Asia/Hong_Kong')"
			},
			"at": {
				"type": "string",
				"description": "ISO datetime for one-time execution (e.g. '2026-02-28T10:30:00')"
			},
			"job_id": {
				"type": "string",
				"description": "Job ID (for remove)"
			}
		},
		"required": ["action"]
	}`)
}

func (t *CronTool) Execute(args map[string]any) (string, error) {
	action, _ := args["action"].(string)

	switch action {
	case "add":
		return t.addJob(args)
	case "list":
		return t.listJobs()
	case "remove":
		jobID, _ := args["job_id"].(string)
		return t.removeJob(jobID)
	default:
		return fmt.Sprintf("Unknown action: %s. Use add, list, or remove.", action), nil
	}
}

func (t *CronTool) addJob(args map[string]any) (string, error) {
	message, _ := args["message"].(string)
	if message == "" {
		return "Error: message is required for add", nil
	}
	if t.Channel == "" || t.ChatID == "" {
		return "Error: no session context (channel/chat_id)", nil
	}

	cronExpr, _ := args["cron_expr"].(string)
	tz, _ := args["tz"].(string)
	atStr, _ := args["at"].(string)
	everySeconds := 0
	if v, ok := args["every_seconds"].(float64); ok {
		everySeconds = int(v)
	}

	var kind string
	var atUnix int64
	deleteAfter := false

	switch {
	case everySeconds > 0:
		kind = "every"
	case cronExpr != "":
		kind = "cron"
	case atStr != "":
		kind = "at"
		dt, err := time.Parse("2006-01-02T15:04:05", atStr)
		if err != nil {
			dt, err = time.Parse(time.RFC3339, atStr)
		}
		if err != nil {
			return fmt.Sprintf("Error: cannot parse 'at' datetime: %v", err), nil
		}
		atUnix = dt.Unix()
		deleteAfter = true
	default:
		return "Error: provide one of every_seconds, cron_expr, or at", nil
	}

	name := message
	if len(name) > 30 {
		name = name[:30]
	}

	job := t.Service.AddJob(name, kind, cronExpr, tz, message, t.Channel, t.ChatID, everySeconds, atUnix, deleteAfter)
	return fmt.Sprintf("Created job '%s' (id: %s, %s)", job.Name, job.ID, job.Kind), nil
}

func (t *CronTool) listJobs() (string, error) {
	jobs := t.Service.ListJobs()
	if len(jobs) == 0 {
		return "No scheduled jobs.", nil
	}
	var lines []string
	for _, j := range jobs {
		detail := j.Kind
		switch j.Kind {
		case "cron":
			detail = j.CronExpr
		case "every":
			detail = fmt.Sprintf("every %ds", j.EverySeconds)
		case "at":
			detail = time.Unix(j.AtUnix, 0).Format("2006-01-02 15:04")
		}
		lines = append(lines, fmt.Sprintf("- %s (id: %s, %s) → %s:%s", j.Name, j.ID, detail, j.Channel, j.ChatID))
	}
	return "Scheduled jobs:\n" + strings.Join(lines, "\n"), nil
}

func (t *CronTool) removeJob(jobID string) (string, error) {
	if jobID == "" {
		return "Error: job_id is required for remove", nil
	}
	if t.Service.RemoveJob(jobID) {
		return fmt.Sprintf("Removed job %s", jobID), nil
	}
	return fmt.Sprintf("Job %s not found", jobID), nil
}
