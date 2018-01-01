package builder

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"gopkg.in/src-d/go-git.v4"

	"github.com/vim-volt/volt/cmd/buildinfo"
	"github.com/vim-volt/volt/lockjson"
	"github.com/vim-volt/volt/logger"
	"github.com/vim-volt/volt/pathutil"
	"github.com/vim-volt/volt/plugconf"
)

type symlinkBuilder struct {
	BaseBuilder
}

// TODO: rollback when return err (!= nil)
func (builder *symlinkBuilder) Build(buildInfo *buildinfo.BuildInfo, buildReposMap map[string]*buildinfo.Repos) error {
	// Exit if vim executable was not found in PATH
	if _, err := pathutil.VimExecutable(); err != nil {
		return err
	}

	// Get current profile's repos list
	lockJSON, err := lockjson.Read()
	if err != nil {
		return errors.New("could not read lock.json: " + err.Error())
	}
	profile, reposList, err := builder.getCurrentProfileAndReposList(lockJSON)
	if err != nil {
		return err
	}

	logger.Info("Installing vimrc and gvimrc ...")

	vimDir := pathutil.VimDir()
	vimrcPath := filepath.Join(vimDir, pathutil.Vimrc)
	gvimrcPath := filepath.Join(vimDir, pathutil.Gvimrc)
	err = builder.installVimrcAndGvimrc(
		lockJSON.CurrentProfileName, vimrcPath, gvimrcPath, profile.UseVimrc, profile.UseGvimrc,
	)
	if err != nil {
		return err
	}

	// Mkdir opt dir
	optDir := pathutil.VimVoltOptDir()
	os.MkdirAll(optDir, 0755)
	if !pathutil.Exists(optDir) {
		return errors.New("could not create " + optDir)
	}

	vimExePath, err := pathutil.VimExecutable()
	if err != nil {
		return err
	}

	buildInfo.Repos = make([]buildinfo.Repos, 0, len(reposList))
	done := make(chan actionReposResult, len(reposList))
	for i := range reposList {
		go builder.installRepos(&reposList[i], vimExePath, done)
		// Make build-info.json data
		buildInfo.Repos = append(buildInfo.Repos, buildinfo.Repos{
			Type:    reposList[i].Type,
			Path:    reposList[i].Path,
			Version: reposList[i].Version,
		})
	}
	for i := 0; i < len(reposList); i++ {
		result := <-done
		if result.err != nil {
			return err
		}
		if result.repos != nil {
			logger.Debug("Installing " + string(result.repos.Type) + " repository " + result.repos.Path + " ... Done.")
		}
	}

	// Write bundled plugconf file
	content, merr := plugconf.GenerateBundlePlugconf(reposList)
	if merr.ErrorOrNil() != nil {
		// Return vim script parse errors
		return merr
	}
	os.MkdirAll(filepath.Dir(pathutil.BundledPlugConf()), 0755)
	err = ioutil.WriteFile(pathutil.BundledPlugConf(), content, 0644)
	if err != nil {
		return err
	}

	// Write build-info.json
	return buildInfo.Write()
}

func (builder *symlinkBuilder) installRepos(repos *lockjson.Repos, vimExePath string, done chan actionReposResult) {
	src := pathutil.FullReposPathOf(repos.Path)
	dst := pathutil.PackReposPathOf(repos.Path)
	copied := false
	if repos.Type == lockjson.ReposGitType {
		// Open a repository to determine it is bare repository or not
		r, err := git.PlainOpen(src)
		if err != nil {
			done <- actionReposResult{
				err: fmt.Errorf("repository %q: %s", src, err.Error()),
			}
			return
		}
		cfg, err := r.Config()
		if err != nil {
			done <- actionReposResult{
				err: fmt.Errorf("failed to get repository config of %q: %s", src, err.Error()),
			}
			return
		}
		if cfg.Core.IsBare {
			// * Copy files from git objects under vim dir
			// * Run ":helptags" to generate tags file
			updateDone := make(chan actionReposResult)
			(&copyBuilder{}).updateBareGitRepos(r, src, dst, repos, vimExePath, updateDone)
			result := <-updateDone
			if result.err != nil {
				done <- actionReposResult{err: result.err}
				return
			}
			copied = true
		}
	}
	if !copied {
		// Make symlinks under vim dir
		if err := builder.symlink(src, dst); err != nil {
			done <- actionReposResult{err: err}
			return
		}
		// Run ":helptags" to generate tags file
		if err := builder.helptags(repos.Path, vimExePath); err != nil {
			done <- actionReposResult{err: err}
			return
		}
	}
	done <- actionReposResult{repos: repos}
}

func (*symlinkBuilder) symlink(src, dst string) error {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "mklink", "/J", dst, src).Run()
	}
	return os.Symlink(src, dst)
}