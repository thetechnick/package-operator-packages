//go:build mage
// +build mage

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/mt-sre/devkube/dev"
	"github.com/mt-sre/devkube/magedeps"
)

const (
	defaultImageOrg = "quay.io/nschiede"
)

var (
	// Working directory of the project.
	workDir string
	// Dependency directory.
	depsDir  magedeps.DependencyDirectory
	cacheDir string

	logger           logr.Logger
	containerRuntime string

	// components
	Builder = &builder{}
)

func init() {
	var err error
	// Directories
	workDir, err = os.Getwd()
	if err != nil {
		panic(fmt.Errorf("getting work dir: %w", err))
	}
	cacheDir = path.Join(workDir + "/" + ".cache")
	depsDir = magedeps.DependencyDirectory(path.Join(workDir, ".deps"))
	os.Setenv("PATH", depsDir.Bin()+":"+os.Getenv("PATH"))

	logger = stdr.New(nil)

}

// dependency for all targets requiring a container runtime
func determineContainerRuntime() {
	containerRuntime = os.Getenv("CONTAINER_RUNTIME")
	if len(containerRuntime) == 0 || containerRuntime == "auto" {
		cr, err := dev.DetectContainerRuntime()
		if err != nil {
			panic(err)
		}
		containerRuntime = string(cr)
		logger.Info("detected container-runtime", "container-runtime", containerRuntime)
	}
}

// Building
// --------
type Build mg.Namespace

func (Build) Version() {
	Builder.Version()
}

func (Build) Image(image string) {
	mg.Deps(
		mg.F(Builder.Image, image),
	)
}

func (Build) Images() {
	mg.Deps(
		mg.F(Builder.Image, "permission-claim-operator-manager"),
	)
}

func (Build) PushImage(image string) {
	mg.Deps(
		mg.F(Builder.Push, image),
	)
}

func (Build) PushImages() {
	mg.Deps(
		mg.F(Builder.Push, "permission-claim-operator-manager"),
	)
}

type builder struct {
	branch        string
	shortCommitID string
	version       string
	buildDate     string

	// Build Tags
	ldFlags  string
	imageOrg string
}

// init build variables
func (b *builder) init() error {
	// branch
	branchCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	branchBytes, err := branchCmd.Output()
	if err != nil {
		panic(fmt.Errorf("getting git branch: %w", err))
	}
	b.branch = strings.TrimSpace(string(branchBytes))

	// commit id
	shortCommitIDCmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	shortCommitIDBytes, err := shortCommitIDCmd.Output()
	if err != nil {
		panic(fmt.Errorf("getting git short commit id"))
	}
	b.shortCommitID = strings.TrimSpace(string(shortCommitIDBytes))

	// version
	b.version = strings.TrimSpace(os.Getenv("VERSION"))
	if len(b.version) == 0 {
		b.version = b.shortCommitID
	}

	// image org
	b.imageOrg = os.Getenv("IMAGE_ORG")
	if len(b.imageOrg) == 0 {
		b.imageOrg = defaultImageOrg
	}

	return nil
}

func (b *builder) Version() {
	mg.SerialDeps(
		b.init,
	)

	fmt.Println(b.version)
}

func (b *builder) Image(name string) error {
	return b.buildPackageImage(name)
}

// clean/prepare cache directory
func (b *builder) cleanImageCacheDir(name string) (dir string, err error) {
	imageCacheDir := path.Join(cacheDir, "image", name)
	if err := os.RemoveAll(imageCacheDir); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("deleting image cache: %w", err)
	}
	if err := os.Remove(imageCacheDir + ".tar"); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("deleting image cache: %w", err)
	}
	if err := os.MkdirAll(imageCacheDir, os.ModePerm); err != nil {
		return "", fmt.Errorf("create image cache dir: %w", err)
	}
	return imageCacheDir, nil
}

func (b *builder) buildPackageImage(packageName string) error {
	mg.SerialDeps(
		b.init,
		determineContainerRuntime,
	)

	imageCacheDir, err := b.cleanImageCacheDir(packageName)
	if err != nil {
		return err
	}

	imageTag := b.imageURL(packageName)
	for _, command := range [][]string{
		// Copy files for build environment
		{"cp", "-a",
			packageName + "/.",
			imageCacheDir + "/"},
		{"cp", "-a",
			"package.Containerfile",
			path.Join(imageCacheDir, "Containerfile")},

		// Build image!
		{containerRuntime, "build", "-t", imageTag, imageCacheDir},
		{containerRuntime, "image", "save",
			"-o", imageCacheDir + ".tar", imageTag},
	} {
		if err := sh.Run(command[0], command[1:]...); err != nil {
			return fmt.Errorf("running %q: %w", strings.Join(command, " "), err)
		}
	}
	return nil
}

func (b *builder) Push(imageName string) error {
	mg.SerialDeps(
		mg.F(b.Image, imageName),
	)

	// Login to container registry when running on AppSRE Jenkins.
	if _, ok := os.LookupEnv("JENKINS_HOME"); ok {
		log.Println("running in Jenkins, calling container runtime login")
		if err := sh.Run(containerRuntime,
			"login", "-u="+os.Getenv("QUAY_USER"),
			"-p="+os.Getenv("QUAY_TOKEN"), "quay.io"); err != nil {
			return fmt.Errorf("registry login: %w", err)
		}
	}

	if err := sh.Run(containerRuntime, "push", b.imageURL(imageName)); err != nil {
		return fmt.Errorf("pushing image: %w", err)
	}

	return nil
}

func (b *builder) imageURL(name string) string {
	envvar := strings.ReplaceAll(strings.ToUpper(name), "-", "_") + "_IMAGE"
	if url := os.Getenv(envvar); len(url) != 0 {
		return url
	}
	return b.imageOrg + "/" + name + ":" + b.version
}
