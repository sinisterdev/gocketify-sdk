package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/LiterMC/socket.io"
	"github.com/LiterMC/socket.io/engine.io"
)

const (
	defaultBaseURL   = "https://api.rocketify.app"
	defaultSocketURL = "wss://rocketify.app"
	refreshInterval  = 5 * time.Minute
)

// LogType defines the type of log message
type LogType string

const (
	LogTypeInfo    LogType = "info"
	LogTypeSuccess LogType = "success"
	LogTypeError   LogType = "error"
	LogTypeWarn    LogType = "warn"
)

// Options for the SDK initialization
type Options struct {
	BaseURL              string
	SocketURL            string
	Debug                bool
	APIKey               string
	DisableNotifications bool
	DisableConsoleLog    bool
}

// AppLog represents a log entry
type AppLog struct {
	Message string  `json:"message"`
	Type    LogType `json:"type"`
	Time    int64   `json:"time,omitempty"`
}

// TriggerAction represents an action triggered via the SDK
type TriggerAction struct {
	Action string `json:"action"`
}

// NotificationFilters for notification targeting
type NotificationFilters struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// GenericNotification represents a notification to be sent
type GenericNotification struct {
	Title      string                 `json:"title"`
	Message    string                 `json:"message"`
	ImageURL   string                 `json:"imageUrl,omitempty"`
	FooterText string                 `json:"footerText,omitempty"`
	URL        string                 `json:"url,omitempty"`
	Filters    *NotificationFilters   `json:"filters,omitempty"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
}

// EventCallback is a function type for event handlers
type EventCallback func(interface{})

// SDK is the main Rocketify SDK client
type SDK struct {
	options      Options
	config       interface{}
	storage      interface{}
	httpClient   *http.Client
	socketClient *socket.Socket
	engineIO     *engine.Socket
	ctx          context.Context
	cancel       context.CancelFunc
	mutex        sync.RWMutex
	handlers     map[string][]EventCallback
	debug        bool
}

// New creates a new SDK instance
func New() *SDK {
	ctx, cancel := context.WithCancel(context.Background())

	return &SDK{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		ctx:      ctx,
		cancel:   cancel,
		handlers: make(map[string][]EventCallback),
	}
}

// Init initializes the SDK with the provided options
func (s *SDK) Init(opts Options) (interface{}, error) {
	if opts.APIKey == "" {
		return nil, errors.New("please provide an API key")
	}

	s.options = opts
	s.debug = opts.Debug

	// Use default URLs if not provided
	if s.options.BaseURL == "" {
		s.options.BaseURL = defaultBaseURL
	}

	if s.options.SocketURL == "" {
		s.options.SocketURL = defaultSocketURL
	}

	s.debugLog("SDK initialized with options:", opts)

	// Initialize Socket.IO connection
	if err := s.initSocketIO(); err != nil {
		return nil, fmt.Errorf("failed to initialize socket.io: %w", err)
	}

	// Fetch initial settings and storage
	var settings interface{}
	var settingsErr, storageErr error

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		settings, settingsErr = s.updateSettings()
	}()

	wg.Wait()

	if settingsErr != nil {
		return nil, fmt.Errorf("failed to fetch settings: %w", settingsErr)
	}

	if storageErr != nil {
		return nil, fmt.Errorf("failed to fetch storage: %w", storageErr)
	}

	// Start periodic data fetching
	go s.periodicallyFetchUpdatedData()

	// Emit ready event
	s.emit("ready", nil)

	return settings, nil
}

// initSocketIO initializes the Socket.IO connection
func (s *SDK) initSocketIO() error {
	var err error
	s.engineIO, err = engine.NewSocket(engine.Options{
		Host:   s.options.SocketURL[6:], // Remove 'wss://' prefix
		Path:   "/socket.io/",
		Secure: true,
		ExtraHeaders: map[string][]string{
			"Content-Type": {"application/json"},
		},
		DialTimeout: time.Minute * 1,
	})

	if err != nil {
		return fmt.Errorf("could not parse Engine.IO options: %w", err)
	}

	// Set up Engine.IO event handlers
	s.engineIO.OnConnect(func(*engine.Socket) {
		s.debugLog("Engine.IO connected")
	})

	s.engineIO.OnDisconnect(func(_ *engine.Socket, err error) {
		if err != nil {
			s.debugLog("Engine.IO disconnected:", err)
		} else {
			s.debugLog("Engine.IO disconnected")
		}
	})

	// Create Socket.IO client
	s.socketClient = socket.NewSocket(s.engineIO, socket.WithAuth(map[string]any{
		"apiKey": func() (string, error) {
			return s.options.APIKey, nil
		},
	}))

	// Set up Socket.IO event handlers
	s.socketClient.OnConnect(func(_ *socket.Socket, _ string) {
		s.debugLog("Connected to Socket.IO server")
	})

	s.socketClient.OnDisconnect(func(_ *socket.Socket, reason string) {
		s.debugLog("Socket.IO disconnected:", reason)
	})

	s.socketClient.OnError(func(_ *socket.Socket, err error) {
		s.debugLog("Socket.IO error:", err)
	})

	// Register event handlers
	s.socketClient.OnMessage(func(event string, data []any) {
		s.debugLog("Received event:", event)
		s.debugLog("Received message:", data)

		switch event {
		case "update:config":
			if len(data) > 0 {
				s.onConfigUpdated(data[0])
			}
		case "update:storage":
			if len(data) > 0 {
				s.onStorageUpdated(data[0])
			}
		case "trigger":
			if len(data) > 0 {
				if triggerData, ok := data[0].(map[string]interface{}); ok {
					action, _ := triggerData["action"].(string)
					s.onTrigger(TriggerAction{Action: action})
				}
			}

		}
	})

	// Connect to Engine.IO
	s.debugLog("Dialing", s.engineIO.URL().String())
	if err := s.engineIO.Dial(s.ctx); err != nil {
		return fmt.Errorf("dial error: %w", err)
	}

	// Connect to Socket.IO namespace
	s.debugLog("Connecting to socket.io namespace")
	if err := s.socketClient.Connect(""); err != nil {
		return fmt.Errorf("open namespace error: %w", err)
	}

	return nil
}

// Close cleans up resources used by the SDK
func (s *SDK) Close() {
	s.cancel()
	if s.socketClient != nil {
		s.socketClient.Close()
	}
}

// On registers an event handler
func (s *SDK) On(event string, callback EventCallback) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.handlers[event] == nil {
		s.handlers[event] = make([]EventCallback, 0)
	}

	s.handlers[event] = append(s.handlers[event], callback)
}

// emit triggers an event to all registered handlers
func (s *SDK) emit(event string, data interface{}) {
	//log mutex current status
	fmt.Println("About to emit event:", event)

	s.mutex.RLock()
	fmt.Println(event)
	handlers := s.handlers[event]
	s.mutex.RUnlock()

	for _, handler := range handlers {
		go handler(data)
	}
}

// Config returns the current configuration
func (s *SDK) Config() interface{} {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return s.config
}

// Storage returns the current storage
func (s *SDK) Storage() interface{} {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return s.storage
}

// StoreData saves data to the storage
func (s *SDK) StoreData(data interface{}) error {
	s.debugLog("Storing data", data)

	s.mutex.Lock()
	prevStorage := s.storage
	s.storage = data
	s.mutex.Unlock()

	err := s.doRequest("POST", "/apps/sdk/storage", data, nil)
	if err != nil {
		s.mutex.Lock()
		s.storage = prevStorage
		s.mutex.Unlock()

		s.debugLog("Error while storing data:", err)
		return fmt.Errorf("couldn't store data: %w", err)
	}

	return nil
}

// LogBatch sends multiple log entries
func (s *SDK) LogBatch(logs []AppLog) error {
	s.debugLog("Sending batch logs", logs)

	logsToSend := make([]AppLog, 0, len(logs))
	for _, log := range logs {
		s.logToConsole(log)
		if log.Message != "" {
			logsToSend = append(logsToSend, log)
		}
	}

	if len(logsToSend) == 0 {
		return nil
	}

	err := s.doRequest("POST", "/apps/log", logsToSend, nil)
	if err != nil {
		s.debugLog("Error while sending batch logs:", err)
		return fmt.Errorf("couldn't log in batch: %w", err)
	}

	return nil
}

// Log sends a single log entry
func (s *SDK) Log(message string, logType LogType) error {
	if message == "" {
		return errors.New("cannot send an empty message")
	}

	log := AppLog{
		Message: message,
		Type:    logType,
	}

	s.logToConsole(log)
	s.debugLog("Sending log", message, logType)

	err := s.doRequest("POST", "/apps/log", log, nil)
	if err != nil {
		s.debugLog("Error while sending log:", err)
		return fmt.Errorf("couldn't log message: %w", err)
	}

	return nil
}

// Notify sends a notification
func (s *SDK) Notify(notification GenericNotification) error {
	if notification.Title == "" || notification.Message == "" {
		return errors.New("invalid notification provided")
	}

	s.debugLog("Sending generic notification", notification)

	if s.options.DisableNotifications {
		return nil
	}

	var response map[string]interface{}
	err := s.doRequest("POST", "/notifications/generic/create", notification, &response)
	if err != nil {
		s.debugLog("Error while sending generic notification:", err)
		return err
	}

	s.debugLog("Got response from notification create:", response)
	return nil
}

// doRequest performs an HTTP request to the API
func (s *SDK) doRequest(method, path string, body interface{}, result interface{}) error {
	url := s.options.BaseURL + path

	var bodyReader io.Reader
	if body != nil {
		bodyData, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(bodyData)
	}

	req, err := http.NewRequestWithContext(s.ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.options.APIKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyData, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(bodyData))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// updateSettings fetches the latest settings from the API
func (s *SDK) updateSettings() (interface{}, error) {
	var config interface{}
	err := s.doRequest("GET", "/apps/sdk/settings", nil, &config)
	if err != nil {
		return nil, err
	}

	return s.onConfigUpdated(config), nil
}

// updateStorage fetches the latest storage from the API
func (s *SDK) updateStorage() (interface{}, error) {
	var storage interface{}
	err := s.doRequest("GET", "/apps/sdk/storage", nil, &storage)
	if err != nil {
		return nil, err
	}

	return s.onStorageUpdated(storage), nil
}

// periodicallyFetchUpdatedData fetches updated data at regular intervals
func (s *SDK) periodicallyFetchUpdatedData() {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_, settingsErr := s.updateSettings()
			_, storageErr := s.updateStorage()

			if settingsErr != nil {
				s.debugLog("Error while fetching settings:", settingsErr)
			}

			if storageErr != nil {
				s.debugLog("Error while fetching storage:", storageErr)
			}
		}
	}
}

// onConfigUpdated handles config updates
func (s *SDK) onConfigUpdated(newConfig interface{}) interface{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	hasUpdatedConfig := s.config != nil && !reflect.DeepEqual(s.config, newConfig)
	s.config = newConfig

	if hasUpdatedConfig {
		s.debugLog("New config received", newConfig)
		s.emit("configUpdated", newConfig) // legacy
		s.emit("config", newConfig)
	}

	return s.config
}

// onStorageUpdated handles storage updates
func (s *SDK) onStorageUpdated(newStorage interface{}) interface{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	hasUpdatedStorage := s.storage != nil && !reflect.DeepEqual(s.storage, newStorage)
	s.storage = newStorage

	if hasUpdatedStorage {
		s.debugLog("New storage received", newStorage)
		s.emit("storage", newStorage)
	}

	return s.storage
}

// onTrigger handles trigger events
func (s *SDK) onTrigger(trigger TriggerAction) {
	s.debugLog("New trigger received", trigger.Action)
	s.emit("trigger", trigger)
}

// logToConsole logs a message to the console if enabled
func (s *SDK) logToConsole(log AppLog) {
	if s.options.DisableConsoleLog {
		return
	}

	switch log.Type {
	case LogTypeError:
		fmt.Printf("ERROR: %s\n", log.Message)
	case LogTypeInfo:
		fmt.Printf("INFO: %s\n", log.Message)
	case LogTypeWarn:
		fmt.Printf("WARN: %s\n", log.Message)
	default:
		fmt.Printf("LOG: %s\n", log.Message)
	}
}

// debugLog logs a debug message if debugging is enabled
func (s *SDK) debugLog(message string, v ...interface{}) {
	if !s.debug {
		return
	}

	if len(v) > 0 {
		log.Printf("[Rocketify DEBUG] %s %v", message, v)
	} else {
		log.Printf("[Rocketify DEBUG] %s", message)
	}
}
