package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
	esbuild "github.com/evanw/esbuild/pkg/api"
	"github.com/sst/ion/internal/fs"
	"github.com/sst/ion/internal/util"
	"github.com/sst/ion/pkg/runtime"
)

var loaderMap = map[string]api.Loader{
	"js":      api.LoaderJS,
	"jsx":     api.LoaderJSX,
	"ts":      api.LoaderTS,
	"tsx":     api.LoaderTSX,
	"css":     api.LoaderCSS,
	"json":    api.LoaderJSON,
	"text":    api.LoaderText,
	"base64":  api.LoaderBase64,
	"file":    api.LoaderFile,
	"dataurl": api.LoaderDataURL,
	"binary":  api.LoaderBinary,
}

type Runtime struct {
	contexts map[string]esbuild.BuildContext
	results  map[string]esbuild.BuildResult
}

func New() *Runtime {
	return &Runtime{
		contexts: map[string]esbuild.BuildContext{},
		results:  map[string]esbuild.BuildResult{},
	}
}

type Worker struct {
	stdout io.ReadCloser
	stderr io.ReadCloser
	cmd    *exec.Cmd
}

func (w *Worker) Stop() {
	// Terminate the whole process group
	util.TerminateProcess(w.cmd.Process.Pid)
}

func (w *Worker) Logs() io.ReadCloser {
	reader, writer := io.Pipe()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(writer, w.stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(writer, w.stderr)
	}()

	go func() {
		wg.Wait()
		defer writer.Close()
	}()

	return reader
}

type NodeProperties struct {
	Loader    map[string]string `json:"loader"`
	Install   []string
	Banner    string
	ESBuild   esbuild.BuildOptions `json:"esbuild"`
	Minify    bool                 `json:"minify"`
	Format    string               `json:"format"`
	SourceMap bool                 `json:"sourceMap"`
	Splitting bool                 `json:"splitting"`
	Plugins   string               `json:"plugins"`
}

var NODE_EXTENSIONS = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

func (r *Runtime) Build(ctx context.Context, input *runtime.BuildInput) (*runtime.BuildOutput, error) {
	var properties NodeProperties
	json.Unmarshal(input.Warp.Properties, &properties)

	file, ok := r.getFile(input)
	if !ok {
		return nil, fmt.Errorf("Handler not found: %v", input.Warp.Handler)
	}
	filepath.Rel(input.Project.PathRoot(), file)

	isESM := true
	extension := ".mjs"

	if properties.Format == "cjs" {
		isESM = false
		extension = ".cjs"
	}

	rel, err := filepath.Rel(input.Project.PathRoot(), file)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(input.Out(), strings.ReplaceAll(rel, filepath.Ext(rel), extension))

	slog.Info("loader info", "loader", properties.Loader)

	loader := map[string]esbuild.Loader{}

	for key, value := range properties.Loader {
		mapped, ok := loaderMap[value]
		if !ok {
			continue
		}
		loader[key] = mapped
	}

	plugins := []esbuild.Plugin{}
	if properties.Plugins != "" {
		plugins = append(plugins, plugin(properties.Plugins))
	}

	options := esbuild.BuildOptions{
		EntryPoints: []string{file},
		Platform:    esbuild.PlatformNode,
		External: append(
			[]string{
				"sharp", "pg-native",
			},
			properties.Install...,
		),
		Plugins:           plugins,
		Sourcemap:         esbuild.SourceMapLinked,
		Loader:            loader,
		KeepNames:         true,
		Bundle:            true,
		Splitting:         properties.Splitting,
		Metafile:          true,
		Write:             true,
		Outfile:           target,
		MinifyWhitespace:  properties.Minify,
		MinifySyntax:      properties.Minify,
		MinifyIdentifiers: properties.Minify,
	}

	if isESM {
		options.Format = esbuild.FormatESModule
		options.Target = esbuild.ESNext
		options.MainFields = []string{"module", "main"}
		options.Banner = map[string]string{
			"js": strings.Join([]string{
				`import { createRequire as topLevelCreateRequire } from 'module';`,
				`const require = topLevelCreateRequire(import.meta.url);`,
				`import { fileURLToPath as topLevelFileUrlToPath, URL as topLevelURL } from "url"`,
				`const __filename = topLevelFileUrlToPath(import.meta.url)`,
				`const __dirname = topLevelFileUrlToPath(new topLevelURL(".", import.meta.url))`,
				properties.Banner,
			}, "\n"),
		}
	} else {
		options.Format = esbuild.FormatCommonJS
		options.Target = esbuild.ESNext
	}

	if properties.ESBuild.Target != 0 {
		options.Target = properties.ESBuild.Target
	}

	buildContext, ok := r.contexts[input.Warp.FunctionID]
	if !ok {
		buildContext, _ = esbuild.Context(options)
		r.contexts[input.Warp.FunctionID] = buildContext
	}

	result := buildContext.Rebuild()
	r.results[input.Warp.FunctionID] = result
	errors := []string{}
	for _, error := range result.Errors {
		text := error.Text
		if error.Location != nil {
			text = text + " " + error.Location.File + ":" + fmt.Sprint(error.Location.Line) + ":" + fmt.Sprint(error.Location.Column)
		}
		errors = append(errors, text)
	}
	for _, error := range result.Errors {
		slog.Error("esbuild error", "error", error)
	}
	for _, warning := range result.Warnings {
		slog.Error("esbuild error", "error", warning)
	}

	nodeModules, err := fs.FindUp(file, "node_modules")
	if err == nil {
		os.Symlink(nodeModules, filepath.Join(input.Out(), "node_modules"))
	}

	return &runtime.BuildOutput{
		Handler: input.Warp.Handler,
		Errors:  errors,
	}, nil
}

func (r *Runtime) Run(ctx context.Context, input *runtime.RunInput) (runtime.Worker, error) {
	cmd := exec.CommandContext(
		ctx,
		"node",
		"--enable-source-maps",
		filepath.Join(
			input.Project.PathPlatformDir(),
			"/dist/nodejs-runtime/index.js",
		),
		filepath.Join(input.Build.Out, input.Build.Handler),
		input.WorkerID,
	)
	util.SetProcessGroupID(cmd)
	cmd.Cancel = func() error {
		return util.TerminateProcess(cmd.Process.Pid)
	}

	cmd.Env = input.Env
	cmd.Env = append(cmd.Env, "NODE_OPTIONS="+os.Getenv("NODE_OPTIONS"))
	cmd.Env = append(cmd.Env, "VSCODE_INSPECTOR_OPTIONS="+os.Getenv("VSCODE_INSPECTOR_OPTIONS"))
	cmd.Env = append(cmd.Env, "AWS_LAMBDA_RUNTIME_API="+input.Server)
	slog.Info("starting worker", "env", cmd.Env, "args", cmd.Args)
	cmd.Dir = input.Build.Out
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	cmd.Start()
	return &Worker{
		stdout,
		stderr,
		cmd,
	}, nil
}

func (r *Runtime) Match(runtime string) bool {
	return strings.HasPrefix(runtime, "node")
}

func (r *Runtime) getFile(input *runtime.BuildInput) (string, bool) {
	dir := filepath.Dir(input.Warp.Handler)
	fileSplit := strings.Split(filepath.Base(input.Warp.Handler), ".")
	base := strings.Join(fileSplit[:len(fileSplit)-1], ".")
	for _, ext := range NODE_EXTENSIONS {
		file := filepath.Join(input.Project.PathRoot(), dir, base+ext)
		if _, err := os.Stat(file); err == nil {
			return file, true
		}
	}
	return "", false
}

func (r *Runtime) ShouldRebuild(functionID string, file string) bool {
	result, ok := r.results[functionID]
	if !ok {
		return false
	}

	var meta = map[string]interface{}{}
	err := json.Unmarshal([]byte(result.Metafile), &meta)
	if err != nil {
		return false
	}
	for key := range meta["inputs"].(map[string]interface{}) {
		absPath, err := filepath.Abs(key)
		if err != nil {
			continue
		}
		if absPath == file {
			return true
		}
	}

	return false
}