// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

// Package packages provides api functions for managing zarf packages
package packages

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/defenseunicorns/zarf/src/config"
	"github.com/defenseunicorns/zarf/src/internal/api/common"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/utils"
	"github.com/defenseunicorns/zarf/src/types"
	"github.com/go-chi/chi/v5"
	"github.com/mholt/archiver/v3"
)

// Read reads a package from the local filesystem and writes the zarf.yaml json to the response.
func Read(w http.ResponseWriter, r *http.Request) {
	message.Debug("packages.Read()")

	path := chi.URLParam(r, "path")

	if pkg, err := readPackage(path); err != nil {
		message.ErrorWebf(err, w, "Unable to read the package")
	} else {
		common.WriteJSONResponse(w, pkg, http.StatusOK)
	}
}

// internal function to read a package from the local filesystem
func readPackage(path string) (pkg types.APIZarfPackage, err error) {
	pkg.Path, err = url.QueryUnescape(path)
	if err != nil {
		return pkg, err
	}

	tmpDir, err := utils.MakeTempDir(config.CommonOptions.TempDirectory)
	if err != nil {
		return pkg, fmt.Errorf("unable to create tmpdir:  %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Extract the archive
	err = archiver.Extract(pkg.Path, config.ZarfYAML, tmpDir)
	if err != nil {
		return pkg, err
	}

	// Read the Zarf yaml
	configPath := filepath.Join(tmpDir, config.ZarfYAML)
	err = utils.ReadYaml(configPath, &pkg.ZarfPackage)

	return pkg, err
}
