/*
   Copyright 2019 Splunk Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package commands

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/spf13/cobra"
	"github.com/splunk/qbec/internal/diff"
	"github.com/splunk/qbec/internal/model"
	"github.com/splunk/qbec/internal/objsort"
	"github.com/splunk/qbec/internal/remote"
	"github.com/splunk/qbec/internal/sio"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type diffIgnores struct {
	allAnnotations  bool
	allLabels       bool
	annotationNames []string
	labelNames      []string
}

func (di diffIgnores) preprocess(obj *unstructured.Unstructured) {
	if di.allLabels || len(di.labelNames) > 0 {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		if di.allLabels {
			labels = map[string]string{}
		} else {
			for _, l := range di.labelNames {
				delete(labels, l)
			}
		}
		obj.SetLabels(labels)
	}
	if di.allAnnotations || len(di.annotationNames) > 0 {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if di.allAnnotations {
			annotations = map[string]string{}
		} else {
			for _, l := range di.annotationNames {
				delete(annotations, l)
			}
		}
		obj.SetAnnotations(annotations)
	}
}

type diffStats struct {
	l         sync.Mutex
	Additions []string `json:"additions,omitempty"`
	Changes   []string `json:"changes,omitempty"`
	Deletions []string `json:"deletions,omitempty"`
	SameCount int      `json:"same,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

func (d *diffStats) added(s string) {
	d.l.Lock()
	defer d.l.Unlock()
	d.Additions = append(d.Additions, s)
}

func (d *diffStats) changed(s string) {
	d.l.Lock()
	defer d.l.Unlock()
	d.Changes = append(d.Changes, s)
}

func (d *diffStats) deleted(s string) {
	d.l.Lock()
	defer d.l.Unlock()
	d.Deletions = append(d.Deletions, s)
}

func (d *diffStats) same(s string) {
	d.l.Lock()
	defer d.l.Unlock()
	d.SameCount++
}

func (d *diffStats) errors(s string) {
	d.l.Lock()
	defer d.l.Unlock()
	d.Errors = append(d.Errors, s)
}

func (d *diffStats) done() {
	sort.Strings(d.Additions)
	sort.Strings(d.Changes)
	sort.Strings(d.Errors)
}

// diffClient is the remote interface needed for show operations.
type diffClient interface {
	listClient
	DisplayName(o model.K8sMeta) string
	Get(obj model.K8sMeta) (*unstructured.Unstructured, error)
}

type differ struct {
	w           io.Writer
	client      diffClient
	opts        diff.Options
	stats       diffStats
	ignores     diffIgnores
	showSecrets bool
	verbose     int
}

func (d *differ) names(ob model.K8sQbecMeta) (name, leftName, rightName string) {
	name = d.client.DisplayName(ob)
	leftName = "live " + name
	rightName = "config " + name
	return
}

func (d *differ) fakeDiff(ob model.K8sQbecMeta, leftContent, rightContent string) error {
	w := d.w
	name, leftName, rightName := d.names(ob)
	fileOpts := d.opts
	fileOpts.LeftName = leftName
	fileOpts.RightName = rightName
	b, err := diff.Strings(leftContent, rightContent, fileOpts)
	if err != nil {
		sio.Errorf("error diffing %s, %v\n", name, err)
		d.stats.errors(name)
		return err
	}
	fmt.Fprintln(w, string(b))
	return nil
}

// diff diffs the supplied object with its remote version and writes output to its writer.
// Care must be taken to ensure  that only a single write is made to the writer for every invocation.
// Otherwise output will be interleaved across diffs.
func (d *differ) diff(ob model.K8sLocalObject) error {
	w := d.w
	name, leftName, rightName := d.names(ob)

	remoteObject, err := d.client.Get(ob)
	if err != nil {
		if err == remote.ErrNotFound {
			d.stats.added(name)
			return d.fakeDiff(ob, "", "\nobject doesn't exist on the server")
		}
		d.stats.errors(name)
		sio.Errorf("error fetching %s, %v\n", name, err)
		return err
	}

	left, source := remote.GetPristineVersionForDiff(remoteObject)
	leftName += " (source: " + source + ")"
	right := ob.ToUnstructured()

	if !d.showSecrets {
		left, _ = model.HideSensitiveInfo(left)
		right, _ = model.HideSensitiveInfo(right)
	}

	d.ignores.preprocess(left)
	d.ignores.preprocess(right)

	fileOpts := d.opts
	fileOpts.LeftName = leftName
	fileOpts.RightName = rightName
	b, err := diff.Objects(left, right, fileOpts)
	if err != nil {
		sio.Errorf("error diffing %s, %v\n", name, err)
		d.stats.errors(name)
		return err
	}

	if len(b) == 0 {
		if d.verbose > 0 {
			fmt.Fprintf(w, "%s unchanged\n", name)
		}
		d.stats.same(name)
	} else {
		fmt.Fprintln(w, string(b))
		d.stats.changed(name)
	}
	return nil
}

type diffCommandConfig struct {
	StdOptions
	showDeletions  bool
	showSecrets    bool
	parallel       int
	contextLines   int
	di             diffIgnores
	filterFunc     func() (filterParams, error)
	clientProvider func(env string) (diffClient, error)
}

func doDiff(args []string, config diffCommandConfig) error {
	if len(args) != 1 {
		return newUsageError("exactly one environment required")
	}

	env := args[0]
	if env == model.Baseline {
		return newUsageError("cannot diff baseline environment, use a real environment")
	}
	fp, err := config.filterFunc()
	if err != nil {
		return err
	}

	objects, err := filteredObjects(config, env, fp)
	if err != nil {
		return err
	}

	client, err := config.clientProvider(env)
	if err != nil {
		return err
	}

	var lister lister = &stubLister{}
	if config.showDeletions {
		all, err := allObjects(config, env)
		if err != nil {
			return err
		}
		cf, _ := model.NewComponentFilter(fp.includes, fp.excludes)
		var scope remote.ListQueryScope
		lister, scope, err = newRemoteLister(client, all, config.DefaultNamespace(env))
		if err != nil {
			return err
		}
		lister.start(all, remote.ListQueryConfig{
			Application:     config.App().Name(),
			Environment:     env,
			KindFilter:      fp.kindFilter,
			ComponentFilter: cf,
			ListQueryScope:  scope,
		})
	}

	objects = objsort.Sort(objects, config.SortConfig(client.IsNamespaced))

	// since the 0 value of context is turned to 3 by the diff library,
	// special case to turn 0 into a negative number so that zero means zero.
	if config.contextLines == 0 {
		config.contextLines = -1
	}
	opts := diff.Options{Context: config.contextLines, Colorize: config.Colorize()}

	w := &lockWriter{Writer: config.Stdout()}
	d := &differ{
		w:           w,
		client:      client,
		opts:        opts,
		ignores:     config.di,
		showSecrets: config.showSecrets,
		verbose:     config.Verbosity(),
	}
	dErr := runInParallel(objects, d.diff, config.parallel)

	var listErr error
	if dErr == nil {
		extra, err := lister.results()
		if err != nil {
			listErr = err
		} else {
			for _, ob := range extra {
				name := client.DisplayName(ob)
				d.stats.deleted(name)
				if err := d.fakeDiff(ob, "\nobject doesn't exist locally", ""); err != nil {
					return err
				}
			}
		}
	}

	d.stats.done()
	printStats(d.w, &d.stats)
	numDiffs := len(d.stats.Additions) + len(d.stats.Changes) + len(d.stats.Deletions)

	switch {
	case dErr != nil:
		return dErr
	case listErr != nil:
		return listErr
	case numDiffs > 0:
		return fmt.Errorf("%d object(s) different", numDiffs)
	default:
		return nil
	}
}

func newDiffCommand(op OptionsProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "diff <environment>",
		Short:   "diff one or more components against objects in a Kubernetes cluster",
		Example: diffExamples(),
	}

	config := diffCommandConfig{
		clientProvider: func(env string) (diffClient, error) {
			return op().Client(env)
		},
		filterFunc: addFilterParams(cmd, true),
	}
	cmd.Flags().BoolVar(&config.showDeletions, "show-deletes", true, "include deletions in diff")
	cmd.Flags().IntVar(&config.contextLines, "context", 3, "context lines for diff")
	cmd.Flags().IntVar(&config.parallel, "parallel", 5, "number of parallel routines to run")
	cmd.Flags().BoolVarP(&config.showSecrets, "show-secrets", "S", false, "do not obfuscate secret values in the diff")
	cmd.Flags().BoolVar(&config.di.allAnnotations, "ignore-all-annotations", false, "remove all annotations from objects before diff")
	cmd.Flags().StringArrayVar(&config.di.annotationNames, "ignore-annotation", nil, "remove specific annotation from objects before diff")
	cmd.Flags().BoolVar(&config.di.allLabels, "ignore-all-labels", false, "remove all labels from objects before diff")
	cmd.Flags().StringArrayVar(&config.di.labelNames, "ignore-label", nil, "remove specific label from objects before diff")

	cmd.RunE = func(c *cobra.Command, args []string) error {
		config.StdOptions = op()
		return wrapError(doDiff(args, config))
	}
	return cmd
}
