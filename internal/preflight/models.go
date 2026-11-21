package preflight

import (
	"fmt"
	"io"

	"github.com/tik-choco/furu/internal/cli"
	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/models"
)

func ShowModels(cfg config.Config, cachePath, codexPath string, refresh bool, stdout, stderr io.Writer) {
	showModels(cfg, cachePath, refresh, stdout, stderr, func() (models.Catalog, error) {
		return models.Fetch(codexPath, cli.Command)
	})
}

func showModels(cfg config.Config, cachePath string, refresh bool, stdout, stderr io.Writer, fetch func() (models.Catalog, error)) {
	previous, _ := models.ReadCache(cachePath)
	current := previous
	if refresh {
		var err error
		current, err = models.Refresh(cachePath, previous, fetch)
		if err != nil {
			fmt.Fprintf(stderr, "furu: warning: refresh Codex model catalog: %v\n", err)
		}
		models.PrintChanges(stderr, previous, current)
	}
	models.PrintTable(stdout, cfg, current)
	models.WarnEfforts(stderr, cfg, current)
}
