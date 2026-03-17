package netcraze

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type ScheduleAction struct {
	Type string
	Left time.Duration
}

type Client struct {
	Address   string
	Username  string
	Password  string
	Schedules map[string][]ScheduleAction

	mu sync.Mutex
}

func NewClient(address string, user, pass string) *Client {
	return &Client{
		Address:   address,
		Username:  user,
		Password:  pass,
		Schedules: make(map[string][]ScheduleAction),
	}
}

func parseScheduleLine(line string) map[string]string {
	params := make(map[string]string)

	for part := range strings.SplitSeq(line, ",") {
		part = strings.TrimSpace(part)
		if paramKey, paramValue, found := strings.Cut(part, "="); found {
			params[strings.TrimSpace(paramKey)] = strings.TrimSpace(paramValue)
		} else {
			params[""] = part
		}
	}
	return params
}

func parseDuration(s string) time.Duration {
	var d int
	_, err := fmt.Sscanf(s, "%d", &d)
	if err != nil {
		return 0
	}
	return time.Duration(d) * time.Second
}

func (c *Client) FetchSchedules(ctx context.Context) error {
	config := &ssh.ClientConfig{
		User: c.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(c.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	client, err := ssh.Dial("tcp", c.Address, config)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()

	var b bytes.Buffer
	session.Stdout = &b

	if err := session.Run("show schedule"); err != nil {
		return err
	}

	lines := strings.Split(b.String(), "\n")
	schedules := make(map[string][]ScheduleAction)
	curScheduleName := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimSuffix(line, "\r")
		line, endsWithColon := strings.CutSuffix(line, ":")
		if !endsWithColon || len(line) == 0 {
			continue
		}

		lineParams := parseScheduleLine(line)
		switch lineParams[""] {
		case "schedule":
			curScheduleName = lineParams["name"]
			if curScheduleName == "" {
				return fmt.Errorf("schedule name is empty in line: %s", line)
			}
			schedules[curScheduleName] = make([]ScheduleAction, 0)
		case "action":
			if curScheduleName == "" {
				return fmt.Errorf("schedule name is empty in line: %s", line)
			}
			actionType := lineParams["type"]
			actionLeft := lineParams["left"]
			if actionType == "" || actionLeft == "" {
				return fmt.Errorf("invalid action line: %s", line)
			}
			schedules[curScheduleName] = append(schedules[curScheduleName], ScheduleAction{
				Type: actionType,
				Left: parseDuration(actionLeft),
			})
		}
	}

	for _, actions := range schedules {
		if len(actions) > 1 {
			slices.SortFunc(actions, func(a, b ScheduleAction) int { return int(a.Left.Seconds() - b.Left.Seconds()) })
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.Schedules = schedules

	return nil
}

func (c *Client) IsScheduleActive(ctx context.Context, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	actions, ok := c.Schedules[name]
	if !ok || len(actions) == 0 {
		return false, fmt.Errorf("schedule %s not found", name)
	}
	if len(actions) < 1 {
		return true, nil
	}

	return actions[0].Type == "stop", nil
}
