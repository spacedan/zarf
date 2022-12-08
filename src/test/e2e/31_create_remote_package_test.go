package test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/defenseunicorns/zarf/src/internal/utils"
	"github.com/stretchr/testify/require"
)

func TestRemotePackage(t *testing.T) {
	t.Log("E2E: Config file")
	e2e.setupWithCluster(t)
	defer e2e.teardown(t)

	var (
		path   = fmt.Sprintf("zarf-package-config-file-%s.tar.zst", e2e.arch)
		dir    = "examples/config-file"
		config = "zarf-config.toml"
	)

	e2e.cleanFiles(path, config)

	// Test the config file environment variable
	os.Setenv("ZARF_CONFIG", filepath.Join(dir, config))
	remotePackageTests(t, dir, path)
	os.Unsetenv("ZARF_CONFIG")

	// Test the config file auto-discovery
	utils.CreatePathAndCopy(filepath.Join(dir, config), config)
	remotePackageTests(t, dir, path)

	e2e.cleanFiles(path, config)
}

func remotePackageTests(t *testing.T, dir, path string) {
	stdOut, _, err := e2e.execZarfCommand("package", "create", dir, "--confirm")
	require.NoError(t, err)
	require.Contains(t, string(stdOut), "This is a zebra and they have stripes")
	require.Contains(t, string(stdOut), "This is a leopard and they have spots")
}
