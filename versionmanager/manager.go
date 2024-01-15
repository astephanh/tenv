/*
 *
 * Copyright 2024 gotofuenv authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package versionmanager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"

	"github.com/dvaumoron/gotofuenv/config"
	"github.com/dvaumoron/gotofuenv/pkg/download"
	"github.com/dvaumoron/gotofuenv/pkg/iterate"
	"github.com/dvaumoron/gotofuenv/pkg/zip"
	"github.com/dvaumoron/gotofuenv/versionmanager/semantic"
	"github.com/hashicorp/go-version"
)

var (
	errEmptyVersion = errors.New("empty version")
	errNoCompatible = errors.New("no compatible version found")
)

type ReleaseInfoRetriever interface {
	DownloadAssetsUrl(version string) (string, string, error)
	LatestRelease() (string, error)
	ListReleases() ([]string, error)
}

type SignatureChecker interface {
	Check(data []byte, dataSig []byte) error
}

type VersionManager struct {
	checker         SignatureChecker
	conf            *config.Config
	FolderName      string
	retriever       ReleaseInfoRetriever
	VersionEnvName  string
	VersionFileName string
}

func MakeVersionManager(conf *config.Config, folderName string, retriever ReleaseInfoRetriever, versionEnvName string, versionFileName string) VersionManager {
	return VersionManager{conf: conf, FolderName: folderName, retriever: retriever, VersionEnvName: versionEnvName, VersionFileName: versionFileName}
}

// detect version (can install depending on auto install env var)
func (m VersionManager) Detect() (string, error) {
	configVersion := m.Resolve(config.LatestAllowedKey)
	return m.detect(configVersion)
}

func (m VersionManager) Install(requestedVersion string) error {
	parsedVersion, err := version.NewVersion(requestedVersion)
	if err == nil {
		return m.installSpecificVersion(parsedVersion.String())
	}

	if requestedVersion == config.LatestKey {
		_, err = m.installLatest()
		return err
	}

	predicate, reverseOrder, err := semantic.ParsePredicate(requestedVersion, m.conf.Verbose)
	if err != nil {
		return err
	}
	// noInstall is set to false to force install regardless of conf
	_, err = m.searchInstallRemote(predicate, reverseOrder, false)
	return err
}

// try to ensure the directory exists with a MkdirAll call.
// (made lazy method : not always useful and allows flag override for root path)
func (m VersionManager) InstallPath() string {
	dir := path.Join(m.conf.RootPath, m.FolderName)
	if err := os.MkdirAll(dir, 0755); err != nil && m.conf.Verbose {
		fmt.Println("Can not create installation directory :", err)
	}
	return dir
}

func (m VersionManager) ListLocal() ([]string, error) {
	entries, err := os.ReadDir(m.InstallPath())
	if err != nil {
		return nil, err
	}

	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			versions = append(versions, entry.Name())
		}
	}

	slices.SortFunc(versions, semantic.CmpVersion)
	return versions, nil
}

func (m VersionManager) ListRemote() ([]string, error) {
	versions, err := m.retriever.ListReleases()
	if err != nil {
		return nil, err
	}

	slices.SortFunc(versions, semantic.CmpVersion)
	return versions, nil
}

func (m VersionManager) LocalSet() map[string]struct{} {
	entries, err := os.ReadDir(m.InstallPath())
	if err != nil {
		if m.conf.Verbose {
			fmt.Println("Can not read installed versions :", err)
		}
		return nil
	}

	versionSet := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			versionSet[entry.Name()] = struct{}{}
		}
	}
	return versionSet
}

func (m VersionManager) Reset() error {
	versionFilePath := m.RootVersionFilePath()
	if m.conf.Verbose {
		fmt.Println("Remove", versionFilePath)
	}
	return os.RemoveAll(versionFilePath)
}

// (made lazy method : not always useful and allows flag override for root path)
func (m VersionManager) Resolve(defaultVersion string) string {
	if forcedVersion := os.Getenv(m.VersionEnvName); forcedVersion != "" {
		return forcedVersion
	}

	data, err := os.ReadFile(m.VersionFileName)
	if err == nil {
		return string(bytes.TrimSpace(data))
	}

	data, err = os.ReadFile(path.Join(m.conf.UserPath, m.VersionFileName))
	if err == nil {
		return string(bytes.TrimSpace(data))
	}

	data, err = os.ReadFile(m.RootVersionFilePath())
	if err == nil {
		return string(bytes.TrimSpace(data))
	}
	return defaultVersion
}

// (made lazy method : not always useful and allows flag override for root path)
func (m VersionManager) RootVersionFilePath() string {
	return path.Join(m.conf.RootPath, m.VersionFileName)
}

func (m VersionManager) Uninstall(requestedVersion string) error {
	parsedVersion, err := version.NewVersion(requestedVersion)
	if err != nil {
		return err
	}

	cleanedVersion := parsedVersion.String()
	targetPath := path.Join(m.InstallPath(), cleanedVersion)
	if m.conf.Verbose {
		fmt.Println("Uninstallation of OpenTofu", cleanedVersion, "(Remove directory", targetPath+")")
	}
	return os.RemoveAll(targetPath)
}

func (m VersionManager) Use(requestedVersion string, workingDir bool) error {
	detectedVersion, err := m.detect(requestedVersion)
	if err != nil {
		return err
	}

	targetFilePath := m.VersionFileName
	if !workingDir {
		targetFilePath = m.RootVersionFilePath()
	}
	if m.conf.Verbose {
		fmt.Println("Write", detectedVersion, "in", targetFilePath)
	}
	return os.WriteFile(targetFilePath, []byte(detectedVersion), 0644)
}

func (m VersionManager) detect(requestedVersion string) (string, error) {
	parsedVersion, err := version.NewVersion(requestedVersion)
	if err == nil {
		cleanedVersion := parsedVersion.String()
		if m.conf.NoInstall {
			return cleanedVersion, nil
		}
		return cleanedVersion, m.installSpecificVersion(cleanedVersion)
	}

	predicate, reverseOrder, err := semantic.ParsePredicate(requestedVersion, m.conf.Verbose)
	if err != nil {
		return "", err
	}

	if !m.conf.ForceRemote {
		versions, err := m.ListLocal()
		if err != nil {
			return "", err
		}

		versionReceiver, done := iterate.Iterate(versions, reverseOrder)
		defer done()

		for version := range versionReceiver {
			if predicate(version) {
				return version, nil
			}
		}

		if m.conf.NoInstall {
			return "", errNoCompatible
		} else if m.conf.Verbose {
			fmt.Println("No compatible version found locally, search a remote one...")
		}
	}

	if requestedVersion == config.LatestKey {
		if m.conf.NoInstall {
			return m.retriever.LatestRelease()
		}
		return m.installLatest()
	}
	return m.searchInstallRemote(predicate, reverseOrder, m.conf.NoInstall)
}

func (m VersionManager) installLatest() (string, error) {
	latestVersion, err := m.retriever.LatestRelease()
	if err != nil {
		return "", err
	}
	return latestVersion, m.installSpecificVersion(latestVersion)
}

func (m VersionManager) installSpecificVersion(version string) error {
	if version == "" {
		return errEmptyVersion
	}

	installPath := m.InstallPath()
	entries, err := os.ReadDir(installPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() && version == entry.Name() {
			if m.conf.Verbose {
				fmt.Println("OpenTofu", version, "already installed")
			}
			return nil
		}
	}

	if m.conf.Verbose {
		fmt.Println("Installation of OpenTofu", version)
	}

	downloadUrl, downloadSigUrl, err := m.retriever.DownloadAssetsUrl(version)
	if err != nil {
		return err
	}

	data, err := download.DownloadBytes(downloadUrl)
	if err != nil {
		return err
	}

	dataSig, err := download.DownloadBytes(downloadSigUrl)
	if err != nil {
		return err
	}

	err = m.checker.Check(data, dataSig)
	if err != nil {
		return err
	}

	targetPath := path.Join(installPath, version)
	return zip.UnzipToDir(bytes.NewReader(data), targetPath)
}

func (m VersionManager) searchInstallRemote(predicate func(string) bool, reverseOrder bool, noInstall bool) (string, error) {
	versions, err := m.ListRemote()
	if err != nil {
		return "", err
	}

	versionReceiver, done := iterate.Iterate(versions, reverseOrder)
	defer done()

	for version := range versionReceiver {
		if predicate(version) {
			if noInstall {
				return version, nil
			}
			return version, m.installSpecificVersion(version)
		}
	}
	return "", errNoCompatible
}
