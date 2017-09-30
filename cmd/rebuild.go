package cmd

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vim-volt/go-volt/copyutil"
	"github.com/vim-volt/go-volt/lockjson"
	"github.com/vim-volt/go-volt/logger"
	"github.com/vim-volt/go-volt/pathutil"
	"github.com/vim-volt/go-volt/transaction"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

type rebuildCmd struct{}

func Rebuild(args []string) int {
	// Begin transaction
	err := transaction.Create()
	if err != nil {
		logger.Error("Failed to begin transaction:", err.Error())
		return 10
	}
	defer transaction.Remove()

	cmd := rebuildCmd{}
	err = cmd.doRebuild()
	if err != nil {
		logger.Error("Failed to rebuild:", err.Error())
		return 11
	}

	return 0
}

func (cmd *rebuildCmd) doRebuild() error {
	vimDir := pathutil.VimDir()
	startDir := pathutil.VimVoltStartDir()

	// Read lock.json
	lockJSON, err := lockjson.Read()
	if err != nil {
		return errors.New("could not read lock.json: " + err.Error())
	}

	// Get active profile's repos list
	reposList, err := cmd.getActiveProfileRepos(lockJSON)
	if err != nil {
		return err
	}

	for _, file := range pathutil.LookUpVimrcOrGvimrc() {
		err = cmd.shouldHaveMagicComment(file)
		// If the file does not have magic comment
		if err != nil {
			return errors.New("already exists user vimrc or gvimrc: " + err.Error())
		}
	}

	logger.Info("Rebuilding " + startDir + " directory ...")
	logger.Info("Installing vimrc and gvimrc ...")

	// Install vimrc and gvimrc
	err = cmd.installRCFile(lockJSON.ActiveProfile, "vimrc.vim", filepath.Join(vimDir, "vimrc"))
	if err != nil {
		return err
	}
	err = cmd.installRCFile(lockJSON.ActiveProfile, "gvimrc.vim", filepath.Join(vimDir, "gvimrc"))
	if err != nil {
		return err
	}

	// Remove start dir
	var removeDone <-chan error
	if pathutil.Exists(startDir) {
		var err error
		removeDone, err = cmd.removeStartDir(startDir)
		if err != nil {
			return err
		}
	}
	err = os.MkdirAll(startDir, 0755)
	if err != nil {
		return err
	}

	logger.Info("Installing all repositories files ...")

	// Copy all repositories files to startDir
	copyDone := make(chan copyReposResult, len(reposList))
	for i := range reposList {
		if reposList[i].Type == lockjson.ReposGitType {
			go cmd.copyGitRepos(&reposList[i], startDir, copyDone)
		} else if reposList[i].Type == lockjson.ReposStaticType {
			go cmd.copyStaticRepos(&reposList[i], startDir, copyDone)
		} else {
			copyDone <- copyReposResult{
				errors.New("invalid repository type: " + string(reposList[i].Type)),
				&reposList[i],
			}
		}
	}

	// Wait remove
	var removeErr error
	if removeDone != nil {
		removeErr = <-removeDone
	}

	// Wait copy
	for i := 0; i < len(reposList); i++ {
		result := <-copyDone
		if result.err != nil {
			return errors.New("failed to copy repository '" + result.repos.Path + "': " + result.err.Error())
		}
	}

	// Show remove error
	if removeErr != nil {
		return errors.New("failed to remove '" + startDir + "': " + removeErr.Error())
	}

	return nil
}

func (cmd *rebuildCmd) installRCFile(profileName, srcRCFileName, dst string) error {
	if pathutil.Exists(dst) {
		err := cmd.shouldHaveMagicComment(dst)
		// If the file does not have magic comment
		if err != nil {
			return err
		}
	}

	// Remove destination (~/.vim/vimrc or ~/.vim/gvimrc)
	os.Remove(dst)
	if pathutil.Exists(dst) {
		return errors.New("failed to remove " + dst)
	}

	// Skip if rc file does not exist
	src := pathutil.RCFileOf(profileName, srcRCFileName)
	if !pathutil.Exists(src) {
		return nil
	}

	return cmd.copyFileWithMagicComment(src, dst)
}

const magicComment = "\" NOTE: this file was generated by volt. please modify original file.\n"

// Return error if the magic comment does not exist
func (*rebuildCmd) shouldHaveMagicComment(dst string) error {
	reader, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer reader.Close()

	magic := []byte(magicComment)
	read := make([]byte, len(magic))
	n, err := reader.Read(read)
	if err != nil || n < len(magicComment) {
		return errors.New("'" + dst + "' does not have magic comment")
	}

	for i := range magic {
		if magic[i] != read[i] {
			return errors.New("'" + dst + "' does not have magic comment")
		}
	}
	return nil
}

func (*rebuildCmd) copyFileWithMagicComment(src, dst string) error {
	reader, err := os.Open(src)
	if err != nil {
		return err
	}
	defer reader.Close()

	writer, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer writer.Close()

	_, err = writer.Write([]byte(magicComment))
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, reader)
	return err
}

func (cmd *rebuildCmd) removeStartDir(startDir string) (<-chan error, error) {
	// Rename startDir to {startDir}.bak
	oldDir := startDir + ".old" + cmd.randomString()
	err := os.Rename(startDir, oldDir)
	if err != nil {
		return nil, err
	}

	logger.Info("Removing " + startDir + " ...")

	// Remove files in parallel
	done := make(chan error, 1)
	go func() {
		err = os.RemoveAll(oldDir)
		done <- err
	}()
	return done, nil
}

func (*rebuildCmd) randomString() string {
	var n uint64
	binary.Read(rand.Reader, binary.LittleEndian, &n)
	return strconv.FormatUint(n, 36)
}

type copyReposResult struct {
	err   error
	repos *lockjson.Repos
}

func (*rebuildCmd) getActiveProfileRepos(lockJSON *lockjson.LockJSON) ([]lockjson.Repos, error) {
	// Find active profile
	profile, err := lockJSON.Profiles.FindByName(lockJSON.ActiveProfile)
	if err != nil {
		// this must not be occurred because lockjson.Read()
		// validates that the matching profile exists
		return nil, err
	}

	return lockJSON.GetReposListByProfile(profile)
}

func (cmd *rebuildCmd) copyGitRepos(repos *lockjson.Repos, startDir string, done chan copyReposResult) {
	src := pathutil.FullReposPathOf(repos.Path)
	dst := filepath.Join(startDir, cmd.encodeReposPath(repos.Path))

	r, err := git.PlainOpen(src)
	if err != nil {
		done <- copyReposResult{
			errors.New("failed to open repository: " + err.Error()),
			repos,
		}
		return
	}

	commit := plumbing.NewHash(repos.Version)
	commitObj, err := r.CommitObject(commit)
	if err != nil {
		done <- copyReposResult{
			errors.New("failed to get HEAD commit object: " + err.Error()),
			repos,
		}
		return
	}

	tree, err := r.TreeObject(commitObj.TreeHash)
	if err != nil {
		done <- copyReposResult{
			errors.New("failed to get tree " + commit.String() + ": " + err.Error()),
			repos,
		}
		return
	}

	err = tree.Files().ForEach(func(file *object.File) error {
		osMode, err := file.Mode.ToOSFileMode()
		if err != nil {
			return errors.New("failed to convert file mode: " + err.Error())
		}

		contents, err := file.Contents()
		if err != nil {
			return errors.New("failed get file contents: " + err.Error())
		}

		filename := filepath.Join(dst, file.Name)
		dir, _ := filepath.Split(filename)
		os.MkdirAll(dir, 0755)
		ioutil.WriteFile(filename, []byte(contents), osMode)
		return nil
	})
	if err != nil {
		done <- copyReposResult{err, repos}
		return
	}

	logger.Info("Installing git repository " + repos.Path + " ... Done.")

	done <- copyReposResult{nil, repos}
}

func (*rebuildCmd) encodeReposPath(reposPath string) string {
	return strings.NewReplacer("_", "__", "/", "_").Replace(reposPath)
}

func (cmd *rebuildCmd) copyStaticRepos(repos *lockjson.Repos, startDir string, done chan copyReposResult) {
	src := pathutil.FullReposPathOf(repos.Path)
	dst := filepath.Join(startDir, cmd.encodeReposPath(repos.Path))

	err := copyutil.CopyDir(src, dst)
	if err != nil {
		done <- copyReposResult{
			errors.New("failed to copy static directory: " + err.Error()),
			repos,
		}
		return
	}

	logger.Info("Installing static directory " + repos.Path + " ... Done.")

	done <- copyReposResult{nil, repos}
}
