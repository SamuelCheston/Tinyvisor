package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"gopkg.in/natefinch/lumberjack.v2"
)

type ScreenManager struct {
	LogDir   string
	mu       sync.Mutex
	sessions map[string]*scriptSession
}

type scriptSession struct {
	cmd    *exec.Cmd
	pty    *os.File
	logger *lumberjack.Logger
	subs   map[chan []byte]struct{}
	mu     sync.Mutex
}

func NewScreenManager(baseDir string) (*ScreenManager, error) {
	logDir := filepath.Join(baseDir, "logs")

	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}

	return &ScreenManager{
		LogDir:   logDir,
		sessions: make(map[string]*scriptSession),
	}, nil
}

func (sm *ScreenManager) Start(id, workDir, command string) error {
	sm.mu.Lock()
	if _, ok := sm.sessions[id]; ok {
		sm.mu.Unlock()
		return fmt.Errorf("session %s already exists", id)
	}
	sm.mu.Unlock()

	logFile := filepath.Join(sm.LogDir, id+".log")
	logger := &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    10, // 10 megabytes
		MaxBackups: 3,
		MaxAge:     7,    // days
		Compress:   true, // disabled by default
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	f, err := pty.Start(cmd)
	if err != nil {
		logger.Close()
		return err
	}

	session := &scriptSession{
		cmd:    cmd,
		pty:    f,
		logger: logger,
		subs:   make(map[chan []byte]struct{}),
	}

	sm.mu.Lock()
	sm.sessions[id] = session
	sm.mu.Unlock()

	go sm.consumeSession(id, session)

	return nil
}

func (sm *ScreenManager) consumeSession(id string, session *scriptSession) {
	defer session.pty.Close()
	defer session.logger.Close()

	buf := make([]byte, 4096)
	for {
		n, err := session.pty.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			// 写入日志（带轮转）
			_, _ = session.logger.Write(data)

			// 分发给订阅者
			session.mu.Lock()
			for sub := range session.subs {
				select {
				case sub <- data:
				default:
				}
			}
			session.mu.Unlock()
		}
		if err != nil {
			break
		}
	}

	// 进程结束，清理
	_ = session.cmd.Wait()

	sm.mu.Lock()
	if sm.sessions[id] == session {
		delete(sm.sessions, id)
	}
	sm.mu.Unlock()
}

func (sm *ScreenManager) Stop(id string) error {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return fmt.Errorf("session not found")
	}
	return session.cmd.Process.Signal(syscall.SIGTERM)
}

func (sm *ScreenManager) Kill(id string) error {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return fmt.Errorf("session not found")
	}
	return session.cmd.Process.Kill()
}

func (sm *ScreenManager) SendInput(id string, data []byte) error {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return fmt.Errorf("session not found")
	}
	_, err := session.pty.Write(data)
	return err
}

func (sm *ScreenManager) Resize(id string, cols, rows int) error {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return fmt.Errorf("session not found")
	}
	return pty.Setsize(session.pty, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
}

func (sm *ScreenManager) IsRunning(id string) (bool, error) {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return false, nil
	}
	// 检查进程是否还在运行
	if session.cmd.Process == nil {
		return false, nil
	}
	err := session.cmd.Process.Signal(syscall.Signal(0))
	return err == nil, nil
}

func (sm *ScreenManager) GetPID(id string) (int, error) {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("session not found")
	}
	if session.cmd.Process == nil {
		return 0, fmt.Errorf("process not started")
	}
	return session.cmd.Process.Pid, nil
}

func (sm *ScreenManager) ListRunning() ([]string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	var ids []string
	for id := range sm.sessions {
		ids = append(ids, id)
	}
	return ids, nil
}

// Attach 返回一个用于接收输出的 channel，并处理输入转发
func (sm *ScreenManager) Attach(id string, sub chan []byte) error {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return fmt.Errorf("session not found")
	}

	session.mu.Lock()
	session.subs[sub] = struct{}{}
	session.mu.Unlock()

	return nil
}

func (sm *ScreenManager) Detach(id string, sub chan []byte) {
	sm.mu.Lock()
	session, ok := sm.sessions[id]
	sm.mu.Unlock()
	if !ok {
		return
	}

	session.mu.Lock()
	delete(session.subs, sub)
	session.mu.Unlock()
}

func (sm *ScreenManager) GetLogs(id string) ([]string, error) {
	logFile := filepath.Join(sm.LogDir, id+".log")
	f, err := os.Open(logFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func (sm *ScreenManager) GetRawLogs(id string, maxBytes int64) ([]byte, error) {
	logFile := filepath.Join(sm.LogDir, id+".log")
	info, err := os.Stat(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	size := info.Size()
	if size == 0 {
		return nil, nil
	}

	readSize := size
	if readSize > maxBytes {
		readSize = maxBytes
	}

	f, err := os.Open(logFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if size > maxBytes {
		_, err = f.Seek(size-maxBytes, 0)
		if err != nil {
			return nil, err
		}
	}

	data := make([]byte, readSize)
	_, err = io.ReadFull(f, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}

	return data, nil
}
