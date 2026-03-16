package netcraze

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strings"
	"sync"
	"time"
)

type Client struct {
	Host     string
	Port     int
	Username string
	Password string
	HTTP     *http.Client
	mu       sync.Mutex
}

func NewClient(host string, port int, user, pass string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		Host:     host,
		Port:     port,
		Username: user,
		Password: pass,
		HTTP:     &http.Client{Jar: jar, Timeout: 10 * time.Second},
	}
}

func (c *Client) login(ctx context.Context) error {
	passHash := sha256.Sum256([]byte(c.Password))
	combined := fmt.Sprintf("%x", passHash)
	finalHash := sha256.Sum256([]byte(combined))

	loginData := fmt.Sprintf(`{"login":"%s","password":"%x"}`, c.Username, finalHash)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://%s:%d/auth", c.Host, c.Port),
		strings.NewReader(loginData))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login failed with status: %d", resp.StatusCode)
	}

	return nil
}

type rciRequest []map[string]any

type rciAction struct {
	Type      string `json:"type"` // "start" or "stop"
	DayOfWeek string `json:"dow"`  // Sun, Mon, ..., Sat
	Time      string `json:"time"` // "HH:MM"
	Left      int    `json:"left"` // Seconds to event
}

type rciResponse []struct {
	Show struct {
		Schedule map[string]struct {
			Action []rciAction `json:"action"`
		} `json:"schedule"`
	} `json:"show"`
}

func (c *Client) fetchScheduleNearestAction(ctx context.Context, name string) (rciAction, error) {
	url := fmt.Sprintf("http://%s:%d/rci/", c.Host, c.Port)

	// Create request body: [{"show":{"schedule":{"name":"schedule123"}}}]
	body := rciRequest{
		{
			"show": map[string]any{
				"schedule": map[string]string{
					"name": name,
				},
			},
		},
	}
	jsonData, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return rciAction{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return rciAction{}, ErrUnauthorized
	}

	var rciResp rciResponse
	if err := json.NewDecoder(resp.Body).Decode(&rciResp); err != nil {
		return rciAction{}, err
	}

	if len(rciResp) > 0 {
		scheduleData, exists := rciResp[0].Show.Schedule[name]
		if !exists || len(scheduleData.Action) == 0 {
			return rciAction{}, fmt.Errorf("schedule %s not found or empty", name)
		}

		slices.SortFunc(scheduleData.Action, func(a, b rciAction) int {
			return a.Left - b.Left
		})
		firstAction := scheduleData.Action[0]
		return firstAction, nil
	}

	return rciAction{}, fmt.Errorf("empty response from router")
}

var ErrUnauthorized = errors.New("netcraze: unauthorized")

func isAuthError(err error) bool {
	return errors.Is(err, ErrUnauthorized)
}

func (c *Client) IsScheduleActive(ctx context.Context, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	action, err := c.fetchScheduleNearestAction(ctx, name)
	if err != nil && isAuthError(err) {
		if err := c.login(ctx); err != nil {
			return false, err
		}
		action, err = c.fetchScheduleNearestAction(ctx, name)
	}

	return action.Type == "stop", err
}
