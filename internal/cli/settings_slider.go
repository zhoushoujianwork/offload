package cli

import (
	"io"

	"github.com/chzyer/readline"
	"github.com/manifoldco/promptui/screenbuf"
)

func sliderLines(cursor int) []string {
	track := "[o]----------------[ ]"
	detail := "Research, lookup and small edits."
	if cursor == 1 {
		track = "[ ]----------------[o]"
		detail = "Also implement, test and document."
	}
	return []string{
		"Offload level",
		"Less offload       More offload",
		track,
		"More offload = less caller-model work.",
		detail,
		"Main agent keeps design and review.",
		"Rule changes need a new agent session.",
		"Left/Right: adjust  Enter: apply",
		"Ctrl-C: cancel",
	}
}

func (p terminalSettingsPrompt) Slider(cursor int) (int, error) {
	c := &readline.Config{Stdin: p.in, Stdout: p.out, Stderr: p.out,
		HistoryLimit: -1, UniqueEditLine: true, InterruptPrompt: "\n", EOFPrompt: "\n"}
	if err := c.Init(); err != nil {
		return 0, err
	}
	c.Stdin = readline.NewCancelableStdin(c.Stdin)
	rl, err := readline.NewEx(c)
	if err != nil {
		return 0, err
	}
	defer rl.Close()
	rl.Write([]byte("\x1b[?25l"))
	defer rl.Write([]byte("\x1b[?25h"))
	screen := screenbuf.New(rl)
	defer func() {
		screen.Reset()
		screen.Clear()
		screen.Flush()
	}()
	c.SetListener(func(_ []rune, _ int, key rune) ([]rune, int, bool) {
		switch key {
		case readline.CharEnter, readline.CharInterrupt, readline.CharDelete, readline.CharCtrlJ:
			// readline may already have unblocked Readline; don't redraw during cleanup.
			return nil, 0, true
		case readline.CharBackward:
			cursor = 0
		case readline.CharForward:
			cursor = 1
		}
		width := readline.GetScreenWidth()
		if width <= 0 {
			width = 80
		}
		for _, line := range sliderLines(cursor) {
			screen.WriteString(settingsLine(line, max(1, width-1)))
		}
		screen.Flush()
		return nil, 0, true
	})
	_, err = rl.Readline()
	if err == io.EOF || err == readline.ErrInterrupt {
		return 0, err
	}
	return cursor, err
}
