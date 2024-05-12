package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"

	"github.com/charmbracelet/log"
	"go.abhg.dev/gs/internal/git"
	"go.abhg.dev/gs/internal/gs"
	"go.abhg.dev/gs/internal/must"
)

type downstackEditCmd struct {
	Editor string `env:"EDITOR" help:"Editor to use for editing the downstack."`

	Name string `arg:"" optional:"" help:"Name of the branch to start editing from."`
}

func (cmd *downstackEditCmd) Run(ctx context.Context, log *log.Logger, opts *globalOptions) error {
	if cmd.Editor == "" {
		return errors.New("an editor is required: use --editor or set $EDITOR")
	}

	repo, err := git.Open(ctx, ".", git.OpenOptions{
		Log: log,
	})
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	store, err := ensureStore(ctx, repo, log, opts)
	if err != nil {
		return err
	}

	if cmd.Name == "" {
		currentBranch, err := repo.CurrentBranch(ctx)
		if err != nil {
			return fmt.Errorf("get current branch: %w", err)
		}
		cmd.Name = currentBranch
	}

	if cmd.Name == store.Trunk() {
		return errors.New("cannot edit below trunk")
	}

	svc := gs.NewService(repo, store, log)

	downstacks, err := svc.ListDownstack(ctx, cmd.Name)
	if err != nil {
		return fmt.Errorf("list downstack: %w", err)
	}
	must.NotBeEmptyf(downstacks, "downstack cannot be empty")
	must.BeEqualf(downstacks[0], cmd.Name,
		"downstack must start with the original branch")

	if len(downstacks) == 1 {
		log.Infof("nothing to edit below %s", cmd.Name)
		return nil
	}

	originalBranches := make(map[string]struct{}, len(downstacks))
	for _, branch := range downstacks {
		originalBranches[branch] = struct{}{}
	}

	instructionFile, err := createEditFile(downstacks)
	if err != nil {
		return err
	}

	editCmd := exec.CommandContext(ctx, cmd.Editor, instructionFile)
	editCmd.Stdin = os.Stdin
	editCmd.Stdout = os.Stdout
	editCmd.Stderr = os.Stderr
	if err := editCmd.Run(); err != nil {
		return fmt.Errorf("run editor: %w", err)
	}

	f, err := os.Open(instructionFile)
	if err != nil {
		return fmt.Errorf("open edited file: %w", err)
	}

	newOrder := make([]string, 0, len(downstacks))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		bs := bytes.TrimSpace(scanner.Bytes())
		if len(bs) == 0 || bs[0] == '#' {
			continue
		}

		name := string(bs)
		if _, ok := originalBranches[name]; !ok {
			// TODO: better error
			return fmt.Errorf("branch %q not in original downstack, or is duplicated", name)
		}
		delete(originalBranches, name)

		newOrder = append(newOrder, name)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read edited file: %w", err)
	}

	if len(newOrder) == 0 {
		log.Infof("downstack edit aborted or nothing to do")
		return nil
	}
	newTop := newOrder[0]
	slices.Reverse(newOrder)

	base := store.Trunk()
	for _, branch := range newOrder {
		err := (&branchOntoCmd{
			Branch: branch,
			Onto:   base,
		}).Run(ctx, log, opts)
		if err != nil {
			return fmt.Errorf("branch onto %s: %w", branch, err)
		}
		base = branch
	}

	return (&branchCheckoutCmd{
		Name: newTop,
	}).Run(ctx, log, opts)
}

var _editFooter = `
# Edit the order of branches by modifying the list above.
# The branch at the bottom of the list will be merged into trunk first.
# Branches above that will be stacked on top of it in the order they appear.
# Branches deleted from the list will not be modified.
#
# Save and quit the editor to apply the changes.
# Delete all lines in the editor to abort the operation.
`

func createEditFile(branches []string) (_ string, err error) {
	file, err := os.CreateTemp("", "gs-edit-*.txt")
	if err != nil {
		return "", fmt.Errorf("create temporary file: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	for _, branch := range branches {
		if _, err := fmt.Fprintln(file, branch); err != nil {
			return "", fmt.Errorf("write branc: %w", err)
		}
	}

	if _, err := io.WriteString(file, _editFooter); err != nil {
		return "", fmt.Errorf("write footer: %w", err)
	}

	return file.Name(), nil
}