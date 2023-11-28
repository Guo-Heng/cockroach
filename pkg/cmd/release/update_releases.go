// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/testutils/release"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/version"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

var updateReleasesTestFileCmd = &cobra.Command{
	Use:   "update-releases-file",
	Short: "Updates releases file used in mixed-version logic tests and roachtests",
	Long:  "Updates releases file used in mixed-version logic tests and roachtests",
	RunE:  updateReleasesFile,
}

// minVersion corresponds to the minimum version after which we start
// keeping release data for testing purposes.
var minVersion = version.MustParse("v21.2.0")

const (
	// releaseDataURL is the location of the YAML file maintained by the
	// docs team where release information is encoded. This data is used
	// to render the public CockroachDB releases page. We leverage the
	// data in structured format to generate release information used
	// for testing purposes.
	releaseDataURL  = "https://raw.githubusercontent.com/cockroachdb/docs/main/src/current/_data/releases.yml"
	releaseDataFile = "pkg/testutils/release/cockroach_releases.yaml"

	// header is added in the first line of `releaseDataFile` to
	// highlight the fact that the file should not be edited manually,
	// but through this script.
	header = "# DO NOT EDIT THIS FILE MANUALLY! Use `release update-releases-file`.\n"
)

// Release contains the information we extract from the YAML file in
// `releaseDataURL`.
type Release struct {
	Name      string `yaml:"release_name"`
	Series    string `yaml:"major_version"`
	Previous  string `yaml:"previous_release"`
	Withdrawn bool   `yaml:"withdrawn"`
	CloudOnly bool   `yaml:"cloud_only"`
}

// updateReleasesFile downloads the current release data from the docs
// repo and generates the corresponding data expected by the `release`
// package, saving the final result in the `cockroach_releases.yaml`
// file which is then embedded into the binary.
func updateReleasesFile(_ *cobra.Command, _ []string) (retErr error) {
	fmt.Printf("downloading release data from %q\n", releaseDataURL)
	data, err := downloadReleases()
	if err != nil {
		return err
	}
	fmt.Printf("downloaded release data for %d releases\n", len(data))

	result := processReleaseData(data)
	fmt.Printf("generated data for %d release series\n", len(result))

	if err := validateReleaseData(result); err != nil {
		return fmt.Errorf("failed to validate downloaded data: %w", err)
	}
	addCurrentRelease(result)

	fmt.Printf("writing results to %s\n", releaseDataFile)
	if err := saveResults(result); err != nil {
		return err
	}
	fmt.Printf("done\n")
	return nil
}

func processReleaseData(data []Release) map[string]release.Series {
	var filtered []Release
	for _, r := range data {
		// We ignore versions that cannot be parsed; this should
		// correspond to really old beta releases.
		v, err := version.Parse(r.Name)
		if err != nil {
			continue
		}

		// Filter out everything that is older than `minVersion`
		if !v.AtLeast(minVersion) {
			continue
		}

		// For the purposes of the cockroach_releases file, we are only
		// interested in beta and rc pre-releases, as we do not support
		// upgrades from alpha releases.
		if pre := v.PreRelease(); pre != "" && !strings.HasPrefix(pre, "rc") && !strings.HasPrefix(pre, "beta") {
			continue
		}
		// Skip cloud-only releases, because the binaries are not yet publicly available.
		if r.CloudOnly {
			continue
		}

		filtered = append(filtered, r)
	}

	// Sort release information from oldest to newest.
	sort.Slice(filtered, func(i, j int) bool {
		vi := version.MustParse(filtered[i].Name)
		vj := version.MustParse(filtered[j].Name)
		return vi.Compare(vj) < 0
	})

	bySeries := map[string][]Release{}
	previousMap := map[string]string{}
	var currentSeries string
	for _, d := range filtered {
		// If the release series changed, keep track of which release
		// series preceded the current one.
		if d.Series != currentSeries {
			previousMap[d.Series] = currentSeries
			currentSeries = d.Series
		}
		bySeries[d.Series] = append(bySeries[d.Series], d)
	}

	result := map[string]release.Series{}
	for seriesName, releases := range bySeries {
		var withdrawn []string
		for _, r := range releases {
			if r.Withdrawn {
				withdrawn = append(withdrawn, releaseName(r.Name))
			}
		}

		result[releaseName(seriesName)] = release.Series{
			Latest:      releaseName(releases[len(releases)-1].Name),
			Withdrawn:   withdrawn,
			Predecessor: releaseName(previousMap[seriesName]),
		}
	}

	return result
}

// addCurrentRelease adds an entry to the `data` map corresponding to
// the binary version of the current build, if one does not exist. The
// new entry will have no `Latest` information as, in that case, the
// current release series is still in development.
func addCurrentRelease(data map[string]release.Series) {
	currentVersion := version.MustParse(build.BinaryVersion())
	name := fmt.Sprintf("%d.%d", currentVersion.Major(), currentVersion.Minor())
	if _, ok := data[name]; ok {
		return
	}

	var latestVersion *version.Version
	for _, d := range data {
		v := version.MustParse("v" + d.Latest)
		if latestVersion == nil {
			latestVersion = v
		}

		if v.AtLeast(latestVersion) {
			latestVersion = v
		}
	}

	// Assume that the predecessor of the current version is the latest
	// released series.
	data[name] = release.Series{
		Predecessor: fmt.Sprintf("%d.%d", latestVersion.Major(), latestVersion.Minor()),
	}
}

// validateReleaseData performs a number of validations on the release
// data passed to make sure that we are saving consistent data that
// the `release` package can use.
func validateReleaseData(data map[string]release.Series) error {
	tryParseVersion := func(v string) error {
		_, err := version.Parse("v" + v)
		return err
	}

	var noPredecessors string
	for name, d := range data {
		if d.Predecessor == "" {
			if noPredecessors != "" {
				return fmt.Errorf("two release series without known predecessors: %q and %q", name, noPredecessors)
			}
			noPredecessors = name
		}

		if pred := d.Predecessor; pred != "" {
			if _, ok := data[d.Predecessor]; !ok {
				return fmt.Errorf("predecessor of %q is %q, but there is no release information for it", name, pred)
			}
		}

		if d.Latest == "" {
			return fmt.Errorf("release information for series %q is missing the latest release", name)
		}

		if err := tryParseVersion(d.Latest); err != nil {
			return fmt.Errorf("release information for series %q has invalid latest release %q: %w", name, d.Latest, err)
		}

		for _, w := range d.Withdrawn {
			if err := tryParseVersion(w); err != nil {
				return fmt.Errorf("release information for series %q has invalid withdrawn release %q: %w", name, w, err)
			}
		}

		numReleases := version.MustParse("v"+d.Latest).Patch() + 1
		if len(d.Withdrawn) == numReleases {
			return fmt.Errorf("series %q is invalid: every release has been withdrawn", name)
		}
	}

	return nil
}

func downloadReleases() ([]Release, error) {
	resp, err := httputil.Get(context.Background(), releaseDataURL)
	if err != nil {
		return nil, fmt.Errorf("could not download release data: %w", err)
	}
	defer resp.Body.Close()

	var blob bytes.Buffer
	if _, err := io.Copy(&blob, resp.Body); err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	var data []Release
	if err := yaml.Unmarshal(blob.Bytes(), &data); err != nil { //nolint:yaml
		return nil, fmt.Errorf("failed to YAML parse release data: %w", err)
	}

	return data, nil
}

func saveResults(results map[string]release.Series) (retErr error) {
	f, err := os.CreateTemp("", "releases")
	if err != nil {
		return fmt.Errorf("could not create temporary file: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(f.Name())
		}
	}()

	if _, err := f.Write([]byte(header)); err != nil {
		return fmt.Errorf("error writing comment header: %w", err)
	}

	if err := yaml.NewEncoder(f).Encode(results); err != nil {
		return fmt.Errorf("could not write release data file: %w", err)
	}

	if err := os.Rename(f.Name(), releaseDataFile); err != nil {
		return fmt.Errorf("error moving release data to final destination: %w", err)
	}

	return nil
}

func releaseName(name string) string {
	return strings.TrimPrefix(name, "v")
}
