package workspace_test

import (
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/workspace"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
)

func newWorkspace(t *testing.T, opts *workspace.Opts) *workspace.Workspace {
	te := testenv.GetTestEnv(t)
	root, err := ioutil.TempDir("", "buildbuddy_test_workspace_*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
	})
	ws, err := workspace.New(te, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func writeEmptyFiles(t *testing.T, ws *workspace.Workspace, paths []string) {
	for _, path := range paths {
		fullPath := filepath.Join(ws.Path(), path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0777); err != nil {
			t.Fatal(err)
		}
		if err := ioutil.WriteFile(fullPath, []byte{}, 0777); err != nil {
			t.Fatal(err)
		}
	}
}

func keepmePaths(paths []string) map[string]struct{} {
	expected := map[string]struct{}{}
	for _, path := range paths {
		if strings.Contains(path, "KEEPME") {
			expected[path] = struct{}{}
		}
	}
	return expected
}

func actualFilePaths(t *testing.T, ws *workspace.Workspace) map[string]struct{} {
	paths := map[string]struct{}{}
	err := filepath.WalkDir(ws.Path(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if !d.IsDir() {
			relPath := strings.TrimPrefix(path, ws.Path()+string(os.PathSeparator))
			paths[relPath] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestWorkspaceCleanup_NoPreserveWorkspace_DeletesAllFiles(t *testing.T) {
	filePaths := []string{
		"some_output_directory/DELETEME",
		"some/nested/output/directory/DELETEME",
		"some_output_file_DELETEME",
		"some/nested/output/file/DELETEME",
		"DELETEME",
		"foo/DELETEME",
		"foo/bar/DELETEME",
	}

	ws := newWorkspace(t, &workspace.Opts{Preserve: false})
	ws.SetTask(&repb.ExecutionTask{
		Command: &repb.Command{
			OutputDirectories: []string{
				"some_output_directory",
				"some/nested/output/directory",
			},
			OutputFiles: []string{
				"some_output_file_DELETEME",
				"some/nested/output/file/DELETEME",
			},
		},
	})
	writeEmptyFiles(t, ws, filePaths)

	err := ws.Clean()

	require.NoError(t, err)
	assert.Empty(t, actualFilePaths(t, ws))
}

func TestWorkspaceCleanup_PreserveWorkspace_PreservesAllFilesExceptOutputs(t *testing.T) {
	filePaths := []string{
		"some_output_directory/DELETEME",
		"some/nested/output/directory/DELETEME",
		"some_output_file_DELETEME",
		"some/nested/output/file/DELETEME",
		"KEEPME",
		"foo/KEEPME",
		"foo/bar/KEEPME",
	}
	ws := newWorkspace(t, &workspace.Opts{Preserve: true})
	ws.SetTask(&repb.ExecutionTask{
		Command: &repb.Command{
			OutputDirectories: []string{
				"some_output_directory",
				"some/nested/output/directory",
			},
			OutputFiles: []string{
				"some_output_file_DELETEME",
				"some/nested/output/file/DELETEME",
			},
		},
	})
	writeEmptyFiles(t, ws, filePaths)

	err := ws.Clean()

	require.NoError(t, err)
	assert.Equal(
		t, keepmePaths(filePaths), actualFilePaths(t, ws),
		"expected all KEEPME filePaths (and no others) in the workspace after cleanup",
	)
}
