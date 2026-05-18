package bot

import (
	"errors"
	"strings"
)

type BgSubCmd int

const (
	BgSubStart BgSubCmd = iota
	BgSubList
	BgSubCancel
	BgSubHelp
)

type BgArgs struct {
	Sub     BgSubCmd
	Prompt  string
	Workdir string
	TaskID  string
}

var (
	ErrBgEmptyPrompt         = errors.New("prompt is empty")
	ErrBgFlagInRequiresValue = errors.New("--in requires a directory")
	ErrBgCancelNeedsID       = errors.New("cancel requires a task id")
)

func parseBgArgs(text string) (BgArgs, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "help") {
		return BgArgs{Sub: BgSubHelp}, nil
	}

	first, rest, _ := strings.Cut(text, " ")
	switch strings.ToLower(first) {
	case "list":
		return BgArgs{Sub: BgSubList}, nil
	case "cancel":
		tid := strings.TrimSpace(rest)
		if tid == "" {
			return BgArgs{}, ErrBgCancelNeedsID
		}

		return BgArgs{Sub: BgSubCancel, TaskID: tid}, nil
	}

	fields := strings.Fields(text)

	var (
		prompt  []string
		workdir string
	)

	for i := 0; i < len(fields); i++ {
		if fields[i] == "--in" {
			if i+1 >= len(fields) {
				return BgArgs{}, ErrBgFlagInRequiresValue
			}

			workdir = fields[i+1]
			i++

			continue
		}

		prompt = append(prompt, fields[i])
	}

	if len(prompt) == 0 {
		return BgArgs{}, ErrBgEmptyPrompt
	}

	return BgArgs{Sub: BgSubStart, Prompt: strings.Join(prompt, " "), Workdir: workdir}, nil
}
