package modelcmd

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"unicode"

	"github.com/tik-choco/furu/internal/cli"
	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
)

func Run(args []string, cfg config.Config, path string, interactive bool, in io.Reader, out, stderr io.Writer) error {
	e := editor{cfg: cfg, path: path, out: out, stderr: stderr, fetch: func() (models.Catalog, error) {
		codexPath, err := exec.LookPath("codex")
		if err != nil {
			return models.Catalog{}, fmt.Errorf("find codex: %w", err)
		}
		return models.Fetch(codexPath, cli.Command)
	}}
	return e.run(args, interactive, in)
}

type editor struct {
	cfg         config.Config
	path        string
	out, stderr io.Writer
	fetch       func() (models.Catalog, error)
	catalog     models.Catalog
	refreshed   bool
}

func (e *editor) run(args []string, interactive bool, in io.Reader) error {
	if len(args) == 0 {
		e.readCatalog()
		e.show()
		if interactive {
			return e.edit(in)
		}
		return nil
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: furu model list")
		}
		e.readCatalog()
		e.list()
	case "set":
		target, backend, change, err := parseSet(args[1:])
		if err != nil {
			return err
		}
		e.readCatalog()
		if change.Model != "" {
			if err := e.validateModel(backend, change.Model); err != nil {
				return err
			}
		}
		if err := e.save(target, backend, change); err != nil {
			return err
		}
	case "reset":
		var target string
		switch len(args) {
		case 1:
		case 2:
			var err error
			target, err = config.ParseTarget(args[1])
			if err != nil {
				return err
			}
		default:
			return errors.New("usage: furu model reset [<target>]")
		}
		cfg, err := config.ResetModels(e.path, target)
		if err != nil {
			return err
		}
		e.cfg = cfg
		e.readCatalog()
		e.show()
	default:
		return fmt.Errorf("unknown model command %q (expected list, set, or reset)", args[0])
	}
	return nil
}

func (e *editor) readCatalog() {
	e.catalog, _ = models.ReadCache(models.CachePath(e.path))
	e.refreshed = false
	if !e.catalog.Fresh() {
		e.refresh()
	}
}

func (e *editor) refresh() {
	e.refreshed = true
	var err error
	e.catalog, err = models.Refresh(models.CachePath(e.path), e.catalog, e.fetch)
	if err != nil {
		fmt.Fprintf(e.stderr, "furu: warning: refresh Codex model catalog: %v\n", err)
	}
}

func (e *editor) show() {
	models.PrintTable(e.out, e.cfg, e.catalog)
	fmt.Fprintln(e.out, "config:", e.path)
	models.WarnEfforts(e.stderr, e.cfg, e.catalog)
}

func (e *editor) list() {
	fmt.Fprintln(e.out, "Codex models (visible):")
	for _, model := range e.catalog.Codex {
		fmt.Fprintf(e.out, "  %s\t%s\n", model.Slug, strings.Join(model.Efforts, ", "))
	}
	fmt.Fprintln(e.out, "Claude families:", strings.Join(models.ClaudeFamilies(), ", "))
	models.WarnEfforts(e.stderr, e.cfg, e.catalog)
}

func (e *editor) save(target, backend string, change config.Model) error {
	cfg, err := config.SetModel(e.path, target, backend, change)
	if err != nil {
		return err
	}
	e.cfg = cfg
	e.show()
	return nil
}

func parseSet(args []string) (target, backend string, change config.Model, err error) {
	if len(args) < 3 || len(args) > 4 || (args[2] == "--effort" && len(args) != 4) {
		err = errors.New("usage: furu model set <target> <backend> <model> [effort] | --effort <effort>")
		return
	}
	if target, err = config.ParseTarget(args[0]); err != nil {
		return
	}
	if backend, err = config.ParseBackend(target, args[1]); err != nil {
		return
	}
	if args[2] != "--effort" {
		if err = validateToken("model", args[2]); err != nil {
			return
		}
		change.Model = args[2]
	}
	if len(args) == 4 {
		if err = validateToken("effort", args[3]); err != nil {
			return
		}
		change.Effort = args[3]
	}
	return
}

func validateToken(kind, value string) error {
	if value == "" || strings.HasPrefix(value, "-") || strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return fmt.Errorf("invalid %s %q", kind, value)
	}
	return nil
}

func (e *editor) validateModel(backend, value string) error {
	if err := validateToken("model", value); err != nil {
		return err
	}
	if backend != "codex" {
		return nil
	}
	if models.IsFamily(value) {
		if _, ok := e.catalog.Resolve(value); !ok {
			fmt.Fprintf(e.stderr, "furu: warning: Codex model family %q could not be resolved\n", value)
		}
		return nil
	}
	if _, ok := e.catalog.Find(value); ok {
		return nil
	}
	// A cached list cannot confirm an exact ID for an unlisted model.
	if !e.refreshed {
		e.refresh()
	}
	if !e.catalog.Available() {
		fmt.Fprintf(e.stderr, "furu: warning: Codex catalog unavailable, could not verify model %q\n", value)
		return nil
	}
	if _, ok := e.catalog.Find(value); !ok {
		return fmt.Errorf("Codex model %q is not in the catalog", value)
	}
	return nil
}
