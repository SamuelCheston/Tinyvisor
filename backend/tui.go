package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type TUI struct {
	app       *tview.Application
	list      *tview.List
	logView   *tview.TextView
	status    *tview.TextView
	minivisor *App
}

func NewTUI(minivisor *App) *TUI {
	t := &TUI{
		app:       tview.NewApplication(),
		minivisor: minivisor,
	}

	t.list = tview.NewList().
		SetSelectedFunc(func(index int, mainText string, secondaryText string, shortcut rune) {
			// 点击列表项的操作，这里可以显示脚本详情
		})
	t.list.SetBorder(true).SetTitle(" Managed Scripts ")

	t.logView = tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			t.app.Draw()
		})
	t.logView.SetBorder(true).SetTitle(" System Logs ")

	t.status = tview.NewTextView().
		SetTextAlign(tview.AlignCenter).
		SetText("Minivisor Interactive Dashboard - Press Ctrl+C to exit | Press G to generate pairing PIN")
	t.status.SetBorder(true)

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(t.status, 3, 1, false).
		AddItem(tview.NewFlex().
			AddItem(t.list, 30, 1, true).
			AddItem(t.logView, 0, 2, false), 0, 1, true)

	t.app.SetRoot(flex, true).SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			t.app.Stop()
			return nil
		}

		if event.Rune() == 'G' || event.Rune() == 'g' {
			if err := t.minivisor.GeneratePairingPIN(false); err != nil {
				t.Log(fmt.Sprintf("生成配对码失败: %v", err))
			} else {
				t.Log("已生成新配对码（10分钟有效）")
			}
			return nil
		}

		return event
	})

	return t
}

func (t *TUI) Run() error {
	go t.updateLoop()
	return t.app.Run()
}

func (t *TUI) updateLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		t.app.QueueUpdateDraw(func() {
			t.list.Clear()
			scripts := t.minivisor.listScripts()
			for _, s := range scripts {
				statusColor := "[green]"
				if s.Status != "running" {
					statusColor = "[red]"
				}
				t.list.AddItem(s.Name, fmt.Sprintf("Status: %s%s[white] | PID: %d", statusColor, s.Status, s.PID), 0, nil)
			}

			_, pinStatus := t.minivisor.PairingStatus()
			t.status.SetText(fmt.Sprintf("Minivisor Interactive Dashboard - Press Ctrl+C to exit | Pairing PIN: %s | Press G to generate pairing PIN", pinStatus))
		})
	}
}

func (t *TUI) GetLogWriter() io.Writer {
	return tview.ANSIWriter(t.logView)
}

func (t *TUI) Log(msg string) {
	fmt.Fprintf(t.logView, "[%s] %s\n", time.Now().Format("15:04:05"), msg)
}
