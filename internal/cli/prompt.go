package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/secure"
	"golang.org/x/term"
)

type Prompter struct {
	input  io.Reader
	output io.Writer
	reader *bufio.Reader
}

func NewPrompter(input io.Reader, output io.Writer) *Prompter {
	return &Prompter{input: input, output: output, reader: bufio.NewReader(input)}
}

func (p *Prompter) Text(label, current string, required bool) (string, error) {
	for {
		if current != "" {
			_, _ = fmt.Fprintf(p.output, "%s [%s]: ", label, current)
		} else {
			_, _ = fmt.Fprintf(p.output, "%s: ", label)
		}
		value, err := p.readLine(false)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = current
		}
		if value != "" || !required {
			return value, nil
		}
		_, _ = fmt.Fprintln(p.output, "A value is required.")
	}
}

func (p *Prompter) Secret(label, current, requiredPrefix string) (string, error) {
	for {
		if current == "" {
			_, _ = fmt.Fprintf(p.output, "%s: ", label)
		} else {
			_, _ = fmt.Fprintf(p.output, "%s [configured %s; Enter to keep]: ", label, secure.Mask(current))
		}
		value, err := p.readLine(true)
		if err != nil {
			return "", err
		}
		if value == "" {
			value = current
		}
		if strings.TrimSpace(value) == "" {
			_, _ = fmt.Fprintln(p.output, "A value is required.")
			continue
		}
		if requiredPrefix != "" && !strings.HasPrefix(value, requiredPrefix) {
			_, _ = fmt.Fprintf(p.output, "The value must begin with %s.\n", requiredPrefix)
			continue
		}
		return value, nil
	}
}

func (p *Prompter) Confirm(label string, defaultValue bool) (bool, error) {
	defaultLabel := "y/N"
	if defaultValue {
		defaultLabel = "Y/n"
	}
	for {
		_, _ = fmt.Fprintf(p.output, "%s [%s]: ", label, defaultLabel)
		value, err := p.readLine(false)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "":
			return defaultValue, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			_, _ = fmt.Fprintln(p.output, "Enter y or n.")
		}
	}
}

func (p *Prompter) CSV(label string, current []string) ([]string, error) {
	currentValue := strings.Join(current, ",")
	if currentValue != "" {
		_, _ = fmt.Fprintf(p.output, "%s [%s; Enter to keep; - to clear]: ", label, currentValue)
	} else {
		_, _ = fmt.Fprintf(p.output, "%s: ", label)
	}
	value, err := p.readLine(false)
	if err != nil {
		return nil, err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = currentValue
	}
	if value == "-" || strings.EqualFold(value, "none") {
		return []string{}, nil
	}
	if value == "" {
		return []string{}, nil
	}
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result, nil
}

func (p *Prompter) readLine(secret bool) (string, error) {
	if secret {
		if file, ok := p.input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			value, err := term.ReadPassword(int(file.Fd()))
			_, _ = fmt.Fprintln(p.output)
			if err != nil {
				return "", fmt.Errorf("read secret: %w", err)
			}
			return string(value), nil
		}
	}
	value, err := p.reader.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(value) <= 0) {
		return "", fmt.Errorf("read terminal input: %w", err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r"), nil
}
