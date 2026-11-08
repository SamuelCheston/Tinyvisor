package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestApp 在临时目录里构造一个不启动 HTTP 服务、可供测试直接驱动
// 的 App（与真实部署相同的 newApp 装配路径）。
func newTestApp(t *testing.T) *App {
	t.Helper()
	base := t.TempDir()
	storePath := filepath.Join(base, "scripts.json")
	if err := os.WriteFile(storePath, []byte(`{"scripts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptFiles := filepath.Join(base, "scripts")
	if err := os.MkdirAll(scriptFiles, 0o755); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(Config{Name: "test", APIKey: "test-key"}, storePath, scriptFiles)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// startCrashing 启动一个立即非零退出的脚本（模拟崩溃），并等待会话被
// consumeSession 清理（进程退出后立即发生，早于观察循环的状态翻转）。
func startCrashing(t *testing.T, app *App, name string) string {
	t.Helper()
	view, err := app.createScript(ScriptPayload{
		Name:          name,
		WorkDir:       t.TempDir(),
		Content:       "exit 1",
		RestartPolicy: RestartNever,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.startScript(view.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		running, _ := app.screenMgr.ListRunning()
		if len(running) == 0 {
			return view.ID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("crashed session was not cleaned up")
	return ""
}

// TestRestartByNameAfterCrashSessionGone 是崩溃后状态的核心回归：
// 进程崩溃后会话立即被清理，而状态要等观察循环下一拍（2s）才翻转。
// 这个窗口内按名重启（编排器使用的路径）必须成功——旧实现在此报
// "session not found" 并拒绝重启。
func TestRestartByNameAfterCrashSessionGone(t *testing.T) {
	app := newTestApp(t)
	id := startCrashing(t, app, "crash")

	resolved, found := app.findIDByName("crash")
	if !found || resolved != id {
		t.Fatalf("findIDByName = %q, %v; want %q, true", resolved, found, id)
	}

	item, _ := app.getScript(id)
	t.Logf("status at restart: %s", item.Status)

	view, err := app.restartScript(resolved)
	if err != nil {
		t.Fatalf("restart after crash must succeed, got: %v", err)
	}
	if view.Status != "running" {
		t.Fatalf("status after restart = %q, want running", view.Status)
	}

	// 收尾：重启后的进程同样会立即退出，停止必须不报错（幂等）。
	if _, err := app.stopScript(id); err != nil {
		t.Fatalf("stop after restart: %v", err)
	}
}

// TestRestartAfterCrashStatusStopped 覆盖崩溃后状态已翻转为 stopped 的
// 情形（观察循环先于重启请求到达）：重启同样必须成功。
func TestRestartAfterCrashStatusStopped(t *testing.T) {
	app := newTestApp(t)
	id := startCrashing(t, app, "crash2")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if item, ok := app.getScript(id); ok && item.Status == "stopped" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if item, _ := app.getScript(id); item.Status != "stopped" {
		t.Fatalf("status did not flip to stopped in time: %s", item.Status)
	}

	view, err := app.restartScript(id)
	if err != nil {
		t.Fatalf("restart after crash (stopped state) must succeed: %v", err)
	}
	if view.Status != "running" {
		t.Fatalf("status after restart = %q, want running", view.Status)
	}
}

// TestStopAfterSessionGoneIsIdempotent 会话已被清理时，停止/强制结束
// 不得报错——目标状态（不在运行）已经达成。
func TestStopAfterSessionGoneIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	id := startCrashing(t, app, "crash3")

	if _, err := app.stopScript(id); err != nil {
		t.Fatalf("stop on a crashed script must not error: %v", err)
	}
	if _, err := app.killScript(id); err != nil {
		t.Fatalf("kill on a crashed script must not error: %v", err)
	}
}
