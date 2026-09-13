package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/mattn/go-isatty"
)

//go:embed dist/*
var frontendContent embed.FS

const (
	configFileName      = "config.json"
	daemonRootDirName   = "daemons"
	scriptsStoreName    = "scripts.json"
	scriptFilesDirName  = "scripts"
	maxBufferedLogLines = 500
)

type RestartPolicy string

const (
	RestartNever         RestartPolicy = "never"
	RestartAlways        RestartPolicy = "always"
	RestartUnlessStopped RestartPolicy = "unless-stopped"
	RestartOnFailure     RestartPolicy = "on-failure"
)

type Config struct {
	Port         int    `json:"port"`
	Name         string `json:"name"`
	APIKey       string `json:"apiKey"`
	PairingPIN   string `json:"pairingPIN"`
	PinCreatedAt string `json:"pinCreatedAt"`
	PinExpiresAt string `json:"pinExpiresAt"`
}

type Script struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	WorkDir       string        `json:"workDir"`
	Content       string        `json:"content"`
	AutoStart     bool          `json:"autoStart"`
	RestartPolicy RestartPolicy `json:"restartPolicy"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
}

type scriptStore struct {
	Scripts []Script `json:"scripts"`
}

type ScriptPayload struct {
	Name          string        `json:"name"`
	WorkDir       string        `json:"workDir"`
	Content       string        `json:"content"`
	AutoStart     bool          `json:"autoStart"`
	RestartPolicy RestartPolicy `json:"restartPolicy"`
}

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`
	Message   string    `json:"message"`
}

type ScriptView struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	WorkDir       string        `json:"workDir"`
	Content       string        `json:"content"`
	AutoStart     bool          `json:"autoStart"`
	RestartPolicy RestartPolicy `json:"restartPolicy"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
	Status        string        `json:"status"`
	PID           int           `json:"pid,omitempty"`
	StartedAt     *time.Time    `json:"startedAt,omitempty"`
}

type ManagedScript struct {
	Config      Script
	Status      string
	PID         int
	StartedAt   *time.Time
	Logs        []LogEntry
	Subscribers map[chan LogEntry]struct{}
	Terminal    chan []byte
	TermSubs    map[chan []byte]struct{}
	ManualStop  bool
}

type App struct {
	config      Config
	configPath  string
	storePath   string
	scriptFiles string
	screenMgr   *ScreenManager

	mu       sync.RWMutex
	scripts  map[string]*ManagedScript
	upgrader websocket.Upgrader

	configMu sync.Mutex
}

func generateRandomString(length int) string {
	b := make([]byte, length/2)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func generateRandomPIN(length int) string {
	const charset = "0123456789"
	result := make([]byte, length)
	for i := range result {
		num, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		result[i] = charset[num.Int64()]
	}
	return string(result)
}

func setupEnvironment() (Config, string, string, bool, error) {
	wd, err := os.Getwd()
	if err != nil {
		return Config{}, "", "", false, err
	}

	// 查找配置文件，先在当前目录找，找不到去上级目录找
	configPath := filepath.Join(wd, configFileName)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		parentConfig := filepath.Join(wd, "..", configFileName)
		if _, err := os.Stat(parentConfig); err == nil {
			configPath = parentConfig
		}
	}

	// 确定数据目录 (daemons) 应该与配置文件在同一级
	daemonRoot := filepath.Join(filepath.Dir(configPath), daemonRootDirName)
	storePath := filepath.Join(daemonRoot, scriptsStoreName)
	scriptFiles := filepath.Join(daemonRoot, scriptFilesDirName)

	if mkdirErr := os.MkdirAll(scriptFiles, 0755); mkdirErr != nil {
		return Config{}, "", "", false, mkdirErr
	}

	createdConfig := false
	if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
		defaultConfig := Config{
			Port:       7891,
			Name:       "Tinyvisor Service",
			APIKey:     generateRandomString(32),
			PairingPIN: generateRandomPIN(4),
		}
		data, marshalErr := json.MarshalIndent(defaultConfig, "", "  ")
		if marshalErr != nil {
			return Config{}, "", "", false, marshalErr
		}
		if writeErr := os.WriteFile(configPath, data, 0644); writeErr != nil {
			return Config{}, "", "", false, writeErr
		}
		createdConfig = true
	}

	if _, statErr := os.Stat(storePath); os.IsNotExist(statErr) {
		emptyStore := scriptStore{Scripts: []Script{}}
		data, marshalErr := json.MarshalIndent(emptyStore, "", "  ")
		if marshalErr != nil {
			return Config{}, "", "", false, marshalErr
		}
		if writeErr := os.WriteFile(storePath, data, 0644); writeErr != nil {
			return Config{}, "", "", false, writeErr
		}
	}

	var config Config
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, "", "", false, err
	}
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return Config{}, "", "", false, err
	}

	// 确保配置中有 APIKey
	updated := false
	if config.APIKey == "" {
		config.APIKey = generateRandomString(32)
		updated = true
	}

	if updated {
		data, _ := json.MarshalIndent(config, "", "  ")
		_ = os.WriteFile(configPath, data, 0644)
	}

	if config.Port == 0 {
		config.Port = 8080
	}
	if strings.TrimSpace(config.Name) == "" {
		config.Name = "Tinyvisor Service"
	}

	return config, storePath, scriptFiles, createdConfig, nil
}

func newApp(config Config, storePath, scriptFiles string) (*App, error) {
	daemonRoot := filepath.Dir(storePath)
	screenMgr, err := NewScreenManager(filepath.Join(daemonRoot, "screen_sessions"))
	if err != nil {
		return nil, err
	}

	app := &App{
		config:      config,
		configPath:  filepath.Join(filepath.Dir(storePath), "..", configFileName),
		storePath:   storePath,
		scriptFiles: scriptFiles,
		screenMgr:   screenMgr,
		scripts:     make(map[string]*ManagedScript),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	if err := app.loadScripts(); err != nil {
		return nil, err
	}

	return app, nil
}

func (a *App) loadScripts() error {
	data, err := os.ReadFile(a.storePath)
	if err != nil {
		return err
	}

	var store scriptStore
	if len(data) > 0 {
		if err := json.Unmarshal(data, &store); err != nil {
			return err
		}
	}

	runningIDs, _ := a.screenMgr.ListRunning()
	runningMap := make(map[string]bool)
	for _, rid := range runningIDs {
		runningMap[rid] = true
	}

	for _, storedScript := range store.Scripts {
		status := "stopped"
		var pid int
		var startedAt *time.Time
		if runningMap[storedScript.ID] {
			status = "running"
			pid, _ = a.screenMgr.GetPID(storedScript.ID)
			now := time.Now()
			startedAt = &now
		}

		a.scripts[storedScript.ID] = &ManagedScript{
			Config:      storedScript,
			Status:      status,
			PID:         pid,
			StartedAt:   startedAt,
			Subscribers: make(map[chan LogEntry]struct{}),
			TermSubs:    make(map[chan []byte]struct{}),
			Logs:        []LogEntry{},
		}

		if status == "running" {
			go a.watchProcess(storedScript.ID)
		}
	}

	return nil
}

func (a *App) saveScripts() error {
	a.mu.RLock()
	scripts := make([]Script, 0, len(a.scripts))
	for _, item := range a.scripts {
		scripts = append(scripts, item.Config)
	}
	a.mu.RUnlock()

	sort.Slice(scripts, func(i, j int) bool {
		return scripts[i].CreatedAt.Before(scripts[j].CreatedAt)
	})

	data, err := json.MarshalIndent(scriptStore{Scripts: scripts}, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(a.storePath, data, 0644)
}

func (a *App) listScripts() []ScriptView {
	a.mu.RLock()
	defer a.mu.RUnlock()

	items := make([]ScriptView, 0, len(a.scripts))
	for _, item := range a.scripts {
		items = append(items, toScriptView(item))
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})

	return items
}

func toScriptView(item *ManagedScript) ScriptView {
	return ScriptView{
		ID:            item.Config.ID,
		Name:          item.Config.Name,
		WorkDir:       item.Config.WorkDir,
		Content:       item.Config.Content,
		AutoStart:     item.Config.AutoStart,
		RestartPolicy: item.Config.RestartPolicy,
		CreatedAt:     item.Config.CreatedAt,
		UpdatedAt:     item.Config.UpdatedAt,
		Status:        item.Status,
		PID:           item.PID,
		StartedAt:     item.StartedAt,
	}
}

func validatePayload(payload ScriptPayload) (ScriptPayload, error) {
	payload.Name = strings.TrimSpace(payload.Name)
	payload.WorkDir = strings.TrimSpace(payload.WorkDir)
	payload.Content = strings.TrimSpace(payload.Content)

	if payload.Name == "" {
		return payload, errors.New("脚本名称不能为空")
	}
	if payload.WorkDir == "" {
		return payload, errors.New("运行目录不能为空")
	}

	workDir, err := filepath.Abs(payload.WorkDir)
	if err != nil {
		return payload, errors.New("运行目录格式无效")
	}

	info, err := os.Stat(workDir)
	if err != nil || !info.IsDir() {
		return payload, errors.New("运行目录不存在或不是目录")
	}

	if payload.Content == "" {
		return payload, errors.New("脚本内容不能为空")
	}

	if payload.RestartPolicy == "" {
		payload.RestartPolicy = RestartNever
	}

	payload.WorkDir = workDir
	return payload, nil
}

func generateID() string {
	return fmt.Sprintf("script-%d", time.Now().UnixNano())
}

func (a *App) createScript(payload ScriptPayload) (ScriptView, error) {
	validated, err := validatePayload(payload)
	if err != nil {
		return ScriptView{}, err
	}

	now := time.Now()
	script := Script{
		ID:            generateID(),
		Name:          validated.Name,
		WorkDir:       validated.WorkDir,
		Content:       validated.Content,
		AutoStart:     validated.AutoStart,
		RestartPolicy: validated.RestartPolicy,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	item := &ManagedScript{
		Config:      script,
		Status:      "stopped",
		Subscribers: make(map[chan LogEntry]struct{}),
		TermSubs:    make(map[chan []byte]struct{}),
		Logs:        []LogEntry{},
	}

	a.mu.Lock()
	a.scripts[script.ID] = item
	a.mu.Unlock()

	if err := a.saveScripts(); err != nil {
		a.mu.Lock()
		delete(a.scripts, script.ID)
		a.mu.Unlock()
		return ScriptView{}, err
	}

	return toScriptView(item), nil
}

func (a *App) updateScript(id string, payload ScriptPayload) (ScriptView, error) {
	validated, err := validatePayload(payload)
	if err != nil {
		return ScriptView{}, err
	}

	a.mu.Lock()
	item, ok := a.scripts[id]
	if !ok {
		a.mu.Unlock()
		return ScriptView{}, errors.New("脚本不存在")
	}
	running := item.Status == "running"

	item.Config.Name = validated.Name
	item.Config.WorkDir = validated.WorkDir
	item.Config.Content = validated.Content
	item.Config.AutoStart = validated.AutoStart
	item.Config.RestartPolicy = validated.RestartPolicy
	item.Config.UpdatedAt = time.Now()
	view := toScriptView(item)
	a.mu.Unlock()

	if err := a.saveScripts(); err != nil {
		return ScriptView{}, err
	}

	if running {
		a.appendLog(id, "system", "脚本配置已更新，运行中的进程不受影响，重启后生效")
	}

	return view, nil
}

func (a *App) deleteScript(id string) error {
	if _, err := a.stopScriptAndWait(id, true); err != nil && err.Error() != "脚本当前未运行" {
		return err
	}

	a.mu.Lock()
	if _, ok := a.scripts[id]; !ok {
		a.mu.Unlock()
		return errors.New("脚本不存在")
	}
	delete(a.scripts, id)
	a.mu.Unlock()

	_ = os.Remove(a.scriptPath(id))
	return a.saveScripts()
}

func (a *App) getScript(id string) (*ManagedScript, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	item, ok := a.scripts[id]
	return item, ok
}

func (a *App) scriptPath(id string) string {
	return filepath.Join(a.scriptFiles, id+".sh")
}

func (a *App) appendLog(id, source, message string) {
	a.mu.Lock()
	item, ok := a.scripts[id]
	if !ok {
		a.mu.Unlock()
		return
	}

	entry := LogEntry{
		Timestamp: time.Now(),
		Source:    source,
		Message:   message,
	}

	item.Logs = append(item.Logs, entry)
	if len(item.Logs) > maxBufferedLogLines {
		item.Logs = item.Logs[len(item.Logs)-maxBufferedLogLines:]
	}

	subscribers := make([]chan LogEntry, 0, len(item.Subscribers))
	for subscriber := range item.Subscribers {
		subscribers = append(subscribers, subscriber)
	}
	a.mu.Unlock()

	for _, subscriber := range subscribers {
		select {
		case subscriber <- entry:
		default:
		}
	}
}

func (a *App) startScript(id string) (ScriptView, error) {
	a.mu.Lock()
	item, ok := a.scripts[id]
	if !ok {
		a.mu.Unlock()
		return ScriptView{}, errors.New("脚本不存在")
	}
	if item.Status == "running" {
		view := toScriptView(item)
		a.mu.Unlock()
		return view, errors.New("脚本已在运行中")
	}

	scriptPath := a.scriptPath(id)
	if err := os.WriteFile(scriptPath, []byte(item.Config.Content), 0755); err != nil {
		a.mu.Unlock()
		return ScriptView{}, err
	}

	// 使用 screen 启动
	err := a.screenMgr.Start(id, item.Config.WorkDir, "bash "+scriptPath)
	if err != nil {
		a.mu.Unlock()
		return ScriptView{}, err
	}

	pid, _ := a.screenMgr.GetPID(id)
	now := time.Now()
	item.Status = "running"
	item.PID = pid
	item.StartedAt = &now
	item.ManualStop = false
	view := toScriptView(item)
	a.mu.Unlock()

	a.appendLog(id, "system", fmt.Sprintf("脚本已在 screen 中启动，PID=%d", view.PID))
	go a.watchProcess(id)

	return view, nil
}

func (a *App) watchProcess(id string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		running, err := a.screenMgr.IsRunning(id)
		if err != nil || !running {
			break
		}
	}

	a.mu.Lock()
	item, ok := a.scripts[id]
	if !ok {
		a.mu.Unlock()
		return
	}
	item.PID = 0
	item.StartedAt = nil
	item.Status = "stopped"

	restartPolicy := item.Config.RestartPolicy
	manualStop := item.ManualStop
	a.mu.Unlock()

	a.appendLog(id, "system", "脚本会话已结束")

	shouldRestart := false
	switch restartPolicy {
	case RestartAlways:
		shouldRestart = true
	case RestartUnlessStopped:
		if !manualStop {
			shouldRestart = true
		}
	case RestartOnFailure:
		shouldRestart = true
	}

	if shouldRestart {
		a.appendLog(id, "system", "根据重启策略，准备重启脚本...")
		time.Sleep(2 * time.Second)

		a.mu.RLock()
		item, ok := a.scripts[id]
		if !ok || item.ManualStop {
			a.mu.RUnlock()
			return
		}
		a.mu.RUnlock()

		if _, err := a.startScript(id); err != nil {
			a.appendLog(id, "system", fmt.Sprintf("自动重启失败: %v", err))
		}
	}
}

func (a *App) stopScript(id string) (ScriptView, error) {
	a.mu.Lock()
	item, ok := a.scripts[id]
	if !ok {
		a.mu.Unlock()
		return ScriptView{}, errors.New("脚本不存在")
	}
	item.ManualStop = true
	status := item.Status
	view := toScriptView(item)
	a.mu.Unlock()

	a.appendLog(id, "system", "收到停止请求")

	if status == "running" {
		if err := a.screenMgr.Stop(id); err != nil {
			return view, err
		}
	}

	return view, nil
}

func (a *App) killScript(id string) (ScriptView, error) {
	a.mu.Lock()
	item, ok := a.scripts[id]
	if !ok {
		a.mu.Unlock()
		return ScriptView{}, errors.New("脚本不存在")
	}
	item.ManualStop = true
	status := item.Status
	view := toScriptView(item)
	a.mu.Unlock()

	a.appendLog(id, "system", "收到强制结束请求")

	if status == "running" {
		if err := a.screenMgr.Kill(id); err != nil {
			return view, err
		}
	}

	return view, nil
}

func (a *App) waitUntilStopped(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		a.mu.RLock()
		item, ok := a.scripts[id]
		status := item.Status
		a.mu.RUnlock()

		if !ok || status != "running" {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("等待脚本停止超时")
		}

		<-ticker.C
	}
}

func (a *App) stopScriptAndWait(id string, forceAfterTimeout bool) (ScriptView, error) {
	view, err := a.stopScript(id)
	if err != nil {
		return view, err
	}

	if waitErr := a.waitUntilStopped(id, 3*time.Second); waitErr == nil {
		return view, nil
	}

	if !forceAfterTimeout {
		return view, errors.New("脚本未在超时时间内退出")
	}

	a.appendLog(id, "system", "脚本未在超时时间内退出，准备强制结束")
	if _, err := a.killScript(id); err != nil && err.Error() != "脚本当前未运行" {
		return view, err
	}

	if err := a.waitUntilStopped(id, 2*time.Second); err != nil {
		return view, err
	}

	return view, nil
}

func (a *App) getLogs(id string) ([]LogEntry, error) {
	a.mu.RLock()
	item, ok := a.scripts[id]
	a.mu.RUnlock()

	if !ok {
		return nil, errors.New("脚本不存在")
	}

	// 合并内存中的系统日志和文件中的终端日志
	var logs []LogEntry
	a.mu.RLock()
	logs = make([]LogEntry, len(item.Logs))
	copy(logs, item.Logs)
	a.mu.RUnlock()

	// 从 screen 日志文件中读取内容
	screenLogs, _ := a.screenMgr.GetLogs(id)
	for _, line := range screenLogs {
		logs = append(logs, LogEntry{
			Timestamp: time.Now(), // 我们不知道精确时间，暂时用现在的时间
			Source:    "terminal",
			Message:   line,
		})
	}

	// 按时间排序
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.Before(logs[j].Timestamp)
	})

	// 限制返回条数
	if len(logs) > maxBufferedLogLines {
		logs = logs[len(logs)-maxBufferedLogLines:]
	}

	return logs, nil
}

func (a *App) subscribe(id string) (chan LogEntry, []LogEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	item, ok := a.scripts[id]
	if !ok {
		return nil, nil, errors.New("脚本不存在")
	}

	ch := make(chan LogEntry, 64)
	item.Subscribers[ch] = struct{}{}

	logs := make([]LogEntry, len(item.Logs))
	copy(logs, item.Logs)
	return ch, logs, nil
}

func (a *App) unsubscribe(id string, ch chan LogEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()

	item, ok := a.scripts[id]
	if !ok {
		close(ch)
		return
	}

	delete(item.Subscribers, ch)
	close(ch)
}

func (a *App) startAutoScripts() {
	a.mu.RLock()
	ids := make([]string, 0, len(a.scripts))
	for id, item := range a.scripts {
		if item.Config.AutoStart {
			ids = append(ids, id)
		}
	}
	a.mu.RUnlock()

	sort.Strings(ids)
	for _, id := range ids {
		if _, err := a.startScript(id); err != nil {
			fmt.Printf("auto start failed for %s: %v\n", id, err)
		}
	}
}

func (a *App) saveCurrentConfig() error {
	a.configMu.Lock()
	cfg := a.config
	a.configMu.Unlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.configPath, data, 0644)
}

func (a *App) GeneratePairingPIN(initial bool) error {
	pin := generateRandomPIN(4)
	now := time.Now()

	a.configMu.Lock()
	a.config.PairingPIN = pin
	a.config.PinCreatedAt = now.Format(time.RFC3339)
	if initial {
		a.config.PinExpiresAt = ""
	} else {
		a.config.PinExpiresAt = now.Add(10 * time.Minute).Format(time.RFC3339)
	}
	a.configMu.Unlock()

	return a.saveCurrentConfig()
}

func (a *App) PairingStatus() (bool, string) {
	a.configMu.Lock()
	cfg := a.config
	a.configMu.Unlock()

	if strings.TrimSpace(cfg.PairingPIN) == "" {
		return false, "未生成"
	}

	if cfg.PinExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, cfg.PinExpiresAt); err == nil {
			if time.Now().After(t) {
				return false, "已过期"
			}
			remain := time.Until(t)
			if remain < 0 {
				remain = 0
			}
			return true, fmt.Sprintf("剩余 %s", remain.Truncate(time.Second))
		}
	}

	return true, "无时限"
}

func (a *App) consumePairingPIN() error {
	a.configMu.Lock()
	a.config.PairingPIN = ""
	a.config.PinCreatedAt = ""
	a.config.PinExpiresAt = ""
	a.configMu.Unlock()

	return a.saveCurrentConfig()
}

func writeSSE(w io.Writer, entry LogEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func (a *App) handleTerminalWS(c *gin.Context) {
	id := c.Param("id")
	a.mu.RLock()
	item, ok := a.scripts[id]
	a.mu.RUnlock()

	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "脚本不存在"})
		return
	}

	conn, err := a.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// 1. 发送历史日志记录
	if rawLogs, err := a.screenMgr.GetRawLogs(id, 100*1024); err == nil && len(rawLogs) > 0 {
		_ = conn.WriteMessage(websocket.BinaryMessage, rawLogs)
	}

	// 2. 订阅实时 raw 数据
	subscriber := make(chan []byte, 128)
	// Attach 会在内部处理日志记录
	if err := a.screenMgr.Attach(id, subscriber); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法附加到终端: " + err.Error()})
		return
	}

	defer a.screenMgr.Detach(id, subscriber)

	// 3. 订阅系统日志
	sysSub := make(chan LogEntry, 32)
	a.mu.Lock()
	item.Subscribers[sysSub] = struct{}{}
	a.mu.Unlock()

	defer a.unsubscribe(id, sysSub)

	// 处理客户端输入
	go func() {
		defer conn.Close()
		for {
			mt, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage || mt == websocket.TextMessage {
				var input struct {
					Type string `json:"type"`
					Data string `json:"data"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if err := json.Unmarshal(message, &input); err == nil {
					if input.Type == "input" {
						_ = a.screenMgr.SendInput(id, []byte(input.Data))
					} else if input.Type == "resize" && input.Cols > 0 && input.Rows > 0 {
						_ = a.screenMgr.Resize(id, input.Cols, input.Rows)
					}
				} else {
					_ = a.screenMgr.SendInput(id, message)
				}
			}
		}
	}()

	// 合并发送
	for {
		select {
		case data, ok := <-subscriber:
			if !ok {
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				return
			}
		case entry, ok := <-sysSub:
			if !ok {
				return
			}
			data, _ := json.Marshal(entry)
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-c.Request.Context().Done():
			return
		}
	}
}

func main() {
	serviceInstall := flag.String("service-install", "", "Install as a system service (systemd or openrc)")
	noTUI := flag.Bool("no-tui", false, "Disable interactive terminal UI")
	flag.Parse()

	if *serviceInstall != "" {
		if err := installService(*serviceInstall); err != nil {
			fmt.Fprintf(os.Stderr, "Error installing service: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	config, storePath, scriptFiles, initialPinReady, err := setupEnvironment()
	if err != nil {
		panic(err)
	}

	app, err := newApp(config, storePath, scriptFiles)
	if err != nil {
		panic(err)
	}

	if initialPinReady {
		_ = app.GeneratePairingPIN(true)
	}

	var tui *TUI
	if !*noTUI && isatty.IsTerminal(os.Stdout.Fd()) {
		tui = NewTUI(app)
		gin.DefaultWriter = tui.GetLogWriter()
		gin.DefaultErrorWriter = tui.GetLogWriter()
	}

	r := gin.Default()
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		// c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Minivisor-Key")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	})

	// Auth Middleware
	r.Use(func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/api/pair" || path == "/ping" || path == "/api/config" || !strings.HasPrefix(path, "/api") {
			c.Next()
			return
		}

		key := c.GetHeader("X-Minivisor-Key")
		if key == "" {
			key = c.Query("key")
		}

		if key != config.APIKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未授权：无效或缺失密钥"})
			return
		}
		c.Next()
	})

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "pong",
		})
	})

	api := r.Group("/api")
	{
		api.GET("/config", func(c *gin.Context) {
			hasPin, pinStatus := app.PairingStatus()
			c.JSON(http.StatusOK, gin.H{
				"port":          app.config.Port,
				"name":          app.config.Name,
				"hasPairingPIN": hasPin,
				"pinStatus":     pinStatus,
			})
		})

		api.POST("/pair", func(c *gin.Context) {
			var payload struct {
				PIN string `json:"pin"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式不正确"})
				return
			}

			payload.PIN = strings.TrimSpace(payload.PIN)
			if payload.PIN == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "PIN 不能为空"})
				return
			}

			currentPIN := strings.TrimSpace(app.config.PairingPIN)
			if currentPIN == "" {
				c.JSON(http.StatusConflict, gin.H{"error": "当前无有效配对码，请在服务器本地生成新配对码"})
				return
			}

			if app.config.PinExpiresAt != "" {
				if t, err := time.Parse(time.RFC3339, app.config.PinExpiresAt); err == nil && time.Now().After(t) {
					c.JSON(http.StatusConflict, gin.H{"error": "配对码已过期，请在服务器本地生成新配对码"})
					return
				}
			}

			if payload.PIN != currentPIN {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "PIN 码不正确"})
				return
			}

			if err := app.consumePairingPIN(); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "清除配对码失败"})
				return
			}

			c.JSON(http.StatusOK, gin.H{"apiKey": app.config.APIKey})
		})

		api.GET("/scripts", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"scripts": app.listScripts()})
		})

		api.POST("/scripts", func(c *gin.Context) {
			var payload ScriptPayload
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式不正确"})
				return
			}

			script, err := app.createScript(payload)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusCreated, gin.H{"script": script})
		})

		api.PUT("/scripts/:id", func(c *gin.Context) {
			var payload ScriptPayload
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式不正确"})
				return
			}

			script, err := app.updateScript(c.Param("id"), payload)
			if err != nil {
				status := http.StatusBadRequest
				if err.Error() == "脚本不存在" {
					status = http.StatusNotFound
				}
				c.JSON(status, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, gin.H{"script": script})
		})

		api.DELETE("/scripts/:id", func(c *gin.Context) {
			err := app.deleteScript(c.Param("id"))
			if err != nil {
				status := http.StatusBadRequest
				if err.Error() == "脚本不存在" {
					status = http.StatusNotFound
				}
				c.JSON(status, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, gin.H{"message": "删除成功"})
		})

		api.POST("/scripts/:id/start", func(c *gin.Context) {
			script, err := app.startScript(c.Param("id"))
			if err != nil {
				status := http.StatusBadRequest
				if err.Error() == "脚本不存在" {
					status = http.StatusNotFound
				}
				if err.Error() == "脚本已在运行中" {
					status = http.StatusConflict
				}
				c.JSON(status, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, gin.H{"script": script})
		})

		api.POST("/scripts/:id/stop", func(c *gin.Context) {
			script, err := app.stopScript(c.Param("id"))
			if err != nil {
				status := http.StatusBadRequest
				if err.Error() == "脚本不存在" {
					status = http.StatusNotFound
				}
				if err.Error() == "脚本当前未运行" {
					status = http.StatusConflict
				}
				c.JSON(status, gin.H{"error": err.Error(), "script": script})
				return
			}

			c.JSON(http.StatusOK, gin.H{"script": script})
		})

		api.POST("/scripts/:id/kill", func(c *gin.Context) {
			script, err := app.killScript(c.Param("id"))
			if err != nil {
				status := http.StatusBadRequest
				if err.Error() == "脚本不存在" {
					status = http.StatusNotFound
				}
				if err.Error() == "脚本当前未运行" {
					status = http.StatusConflict
				}
				c.JSON(status, gin.H{"error": err.Error(), "script": script})
				return
			}

			c.JSON(http.StatusOK, gin.H{"script": script})
		})

		api.GET("/scripts/:id/logs", func(c *gin.Context) {
			logs, err := app.getLogs(c.Param("id"))
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, gin.H{"logs": logs})
		})

		api.GET("/scripts/:id/logs/stream", func(c *gin.Context) {
			subscriber, history, err := app.subscribe(c.Param("id"))
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}
			defer app.unsubscribe(c.Param("id"), subscriber)

			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "流式日志当前不可用"})
				return
			}

			for _, entry := range history {
				if err := writeSSE(c.Writer, entry); err != nil {
					return
				}
			}
			flusher.Flush()

			keepAlive := time.NewTicker(15 * time.Second)
			defer keepAlive.Stop()

			for {
				select {
				case entry := <-subscriber:
					if err := writeSSE(c.Writer, entry); err != nil {
						return
					}
					flusher.Flush()
				case <-keepAlive.C:
					if _, err := fmt.Fprint(c.Writer, ": keep-alive\n\n"); err != nil {
						return
					}
					flusher.Flush()
				case <-c.Request.Context().Done():
					return
				}
			}
		})

		api.GET("/scripts/:id/terminal", app.handleTerminalWS)

		api.GET("/service/status", func(c *gin.Context) {
			c.JSON(http.StatusOK, getServiceStatus())
		})

		api.POST("/service/install", func(c *gin.Context) {
			var payload struct {
				Type string `json:"type"`
			}
			if err := c.ShouldBindJSON(&payload); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求参数"})
				return
			}

			if err := installService(payload.Type); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, gin.H{"message": "服务配置已生成，请按提示完成安装"})
		})
	}

	// 静态文件服务 (SPA 支持)
	staticFS, _ := fs.Sub(frontendContent, "dist")
	staticServer := http.FileServer(http.FS(staticFS))

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		// 检查文件是否存在于嵌入的 FS 中
		if _, err := staticFS.Open(strings.TrimPrefix(path, "/")); err == nil {
			staticServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		// 如果不存在且不是 API 请求，返回 index.html
		if !strings.HasPrefix(path, "/api") && !strings.HasPrefix(path, "/ping") {
			c.Request.URL.Path = "/"
			staticServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "Not Found"})
	})

	app.startAutoScripts()
	address := fmt.Sprintf(":%d", config.Port)
	if tui != nil {
		tui.Log(fmt.Sprintf("%s listening on %s", config.Name, address))

		hasPin, pinStatus := app.PairingStatus()
		if hasPin {
			tui.Log(fmt.Sprintf("Pairing PIN: %s (%s)", app.config.PairingPIN, pinStatus))
		} else {
			tui.Log(fmt.Sprintf("Pairing PIN: %s", pinStatus))
		}

		go func() {
			if err := r.Run(address); err != nil {
				panic(err)
			}
		}()
		if err := tui.Run(); err != nil {
			panic(err)
		}
	} else {
		fmt.Printf("%s listening on %s\n", config.Name, address)

		hasPin, pinStatus := app.PairingStatus()
		if hasPin {
			fmt.Printf("Pairing PIN: %s (%s)\n", app.config.PairingPIN, pinStatus)
		} else {
			fmt.Printf("Pairing PIN: %s\n", pinStatus)
		}

		if err := r.Run(address); err != nil {
			panic(err)
		}
	}
}
