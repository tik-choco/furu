package modelcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
)

func (e *editor) edit(in io.Reader) error {
	p := prompt{in: bufio.NewScanner(in), out: e.out}
	targets := append([]string{"orchestrator"}, config.TierNames()...)
	for {
		choice, err := p.choose("Target (Enter to quit)", targets, targets)
		if errors.Is(err, io.EOF) || (err == nil && choice == "") {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := config.ParseTarget(choice)
		if err != nil {
			fmt.Fprintln(e.out, err)
			continue
		}
		err = e.editTarget(p, target)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (e *editor) editTarget(p prompt, target string) error {
	backends := []string{"codex", "claude"}
	if target == "orchestrator" {
		backends = []string{"claude"}
	}
	choice, err := p.choose("Backend", backends, backends)
	if err != nil || choice == "" {
		return err
	}
	backend, err := config.ParseBackend(target, choice)
	if err != nil {
		fmt.Fprintln(e.out, err)
		return nil
	}
	values := models.ClaudeFamilies()
	labels := values
	if backend == "codex" {
		values, labels = e.codexChoices()
	}
	value, err := p.choose("Model (choose a number or type an ID)", values, labels)
	if err != nil || value == "" {
		return err
	}
	if err := e.validateModel(backend, value); err != nil {
		fmt.Fprintln(e.out, err)
		return nil
	}
	efforts, _ := e.catalog.Efforts(backend, value)
	current := e.cfg.Model(target, backend)
	message := fmt.Sprintf("Effort (Enter to keep %s)", current.Effort)
	effort, err := p.choose(message, efforts, efforts)
	if err != nil {
		return err
	}
	if effort != "" {
		if err := validateToken("effort", effort); err != nil {
			fmt.Fprintln(e.out, err)
			return nil
		}
	}
	return e.save(target, backend, config.Model{Model: value, Effort: effort})
}

func (e *editor) codexChoices() (values, labels []string) {
	for _, family := range e.catalog.Families() {
		values = append(values, family)
		labels = append(labels, e.catalog.Label("codex", family))
	}
	for _, model := range e.catalog.Codex {
		values = append(values, model.Slug)
		labels = append(labels, model.Slug)
	}
	return values, labels
}

type prompt struct {
	in  *bufio.Scanner
	out io.Writer
}

func (p prompt) choose(title string, values, labels []string) (string, error) {
	fmt.Fprintln(p.out, title)
	for i, option := range labels {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, option)
	}
	fmt.Fprint(p.out, "> ")
	if !p.in.Scan() {
		if err := p.in.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	value := strings.TrimSpace(p.in.Text())
	if number, err := strconv.Atoi(value); err == nil && number > 0 && number <= len(values) {
		return values[number-1], nil
	}
	return value, nil
}
