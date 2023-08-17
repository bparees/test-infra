/*
Copyright 2019 The Kubernetes Authors.

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

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gitignore "github.com/denormal/go-gitignore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	gerritsource "k8s.io/test-infra/prow/gerrit/source"

	"k8s.io/test-infra/prow/git/types"
	"k8s.io/test-infra/prow/git/v2"
	"sigs.k8s.io/yaml"
)

const (
	inRepoConfigFileName = ".prow.yaml"
	inRepoConfigDirName  = ".prow"
)

var inrepoconfigRepoOpts = git.RepoOpts{
	// Technically we only need inRepoConfigDirName (".prow") because the
	// default "cone mode" of sparse checkouts already include files at the
	// toplevel (which would include ".prow.yaml").
	//
	// TODO (listx): The version of git shipped in kubekins-e2e (itself
	// derived from the bootstrap image) uses git version 2.30.2, which does
	// not populate files at the toplevel. So we have to also set a sparse
	// checkout of ".prow.yaml". Later when that image is updated, we can
	// remove the use of inRepoConfigFileName (".prow.yaml"), so that the
	// unit tests in CI can pass. As for the Prow components themselves,
	// they use a different version of Git based on alpine (see .ko.yaml in
	// the root).
	SparseCheckoutDirs: []string{inRepoConfigDirName, inRepoConfigFileName},
	// The sparse checkout would avoid creating another copy of Git objects
	// from the mirror clone into the secondary clone.
	ShareObjectsWithSourceRepo: true,
}

var inrepoconfigMetrics = struct {
	gitCloneDuration *prometheus.HistogramVec
	gitOtherDuration *prometheus.HistogramVec
}{
	gitCloneDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inrepoconfig_git_client_acquisition_duration",
		Help:    "Seconds taken for acquiring a git client (may include an initial clone operation).",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 180, 300, 600, 1200, 1800, 3600},
	}, []string{
		"org",
		"repo",
	}),
	gitOtherDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inrepoconfig_git_other_duration",
		Help:    "Seconds taken after acquiring a git client and performing all other git operations (to read the ProwYAML of the repo).",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 180, 300, 600},
	}, []string{
		"org",
		"repo",
	}),
}

func init() {
	prometheus.MustRegister(inrepoconfigMetrics.gitCloneDuration)
	prometheus.MustRegister(inrepoconfigMetrics.gitOtherDuration)
}

// +k8s:deepcopy-gen=true

// ProwYAML represents the content of a .prow.yaml file
// used to version Presubmits and Postsubmits inside the tested repo.
type ProwYAML struct {
	Presets     []Preset     `json:"presets"`
	Presubmits  []Presubmit  `json:"presubmits"`
	Postsubmits []Postsubmit `json:"postsubmits"`

	// ProwIgnored is a well known, unparsed field where non-Prow fields can
	// be defined without conflicting with unknown field validation.
	ProwIgnored *json.RawMessage `json:"prow_ignored,omitempty"`
}

// ProwYAMLGetter is used to retrieve a ProwYAML. Tests should provide
// their own implementation and set that on the Config.
type ProwYAMLGetter func(c *Config, gc git.ClientFactory, identifier, baseSHA string, headSHAs ...string) (*ProwYAML, error)

// Verify prowYAMLGetterWithDefaults and prowYAMLGetter are both of type
// ProwYAMLGetter.
var _ ProwYAMLGetter = prowYAMLGetterWithDefaults
var _ ProwYAMLGetter = prowYAMLGetter

// prowYAMLGetter is like prowYAMLGetterWithDefaults, but without default values
// (it does not call DefaultAndValidateProwYAML()). Its sole purpose is to allow
// caching of ProwYAMLs that are retrieved purely from the inrepoconfig's repo,
// __without__ having the contents modified by the main Config's own settings
// (which happens mostly inside DefaultAndValidateProwYAML()). prowYAMLGetter is
// only used by cache.GetPresubmits() and cache.GetPostsubmits().
func prowYAMLGetter(
	c *Config,
	gc git.ClientFactory,
	identifier string,
	baseSHA string,
	headSHAs ...string) (*ProwYAML, error) {

	log := logrus.WithField("repo", identifier)

	if gc == nil {
		log.Error("prowYAMLGetter was called with a nil git client")
		return nil, errors.New("gitClient is nil")
	}

	orgRepo := *NewOrgRepo(identifier)
	if orgRepo.Repo == "" {
		return nil, fmt.Errorf("didn't get two results when splitting repo identifier %q", identifier)
	}

	timeBeforeClone := time.Now()
	repo, err := gc.ClientForWithRepoOpts(orgRepo.Org, orgRepo.Repo, inrepoconfigRepoOpts)
	inrepoconfigMetrics.gitCloneDuration.WithLabelValues(orgRepo.Org, orgRepo.Repo).Observe((float64(time.Since(timeBeforeClone).Seconds())))
	if err != nil {
		return nil, fmt.Errorf("failed to clone repo for %q: %w", identifier, err)
	}
	timeAfterClone := time.Now()
	defer func() {
		if err := repo.Clean(); err != nil {
			log.WithError(err).Error("Failed to clean up repo.")
		}
		inrepoconfigMetrics.gitOtherDuration.WithLabelValues(orgRepo.Org, orgRepo.Repo).Observe((float64(time.Since(timeAfterClone).Seconds())))
	}()

	if err := repo.Config("user.name", "prow"); err != nil {
		return nil, err
	}
	if err := repo.Config("user.email", "prow@localhost"); err != nil {
		return nil, err
	}
	if err := repo.Config("commit.gpgsign", "false"); err != nil {
		return nil, err
	}

	// TODO(mpherman): This is to hopefully mittigate issue with gerrit merges. Need to come up with a solution that checks
	// each CLs merge strategy as they can differ. ifNecessary is just the gerrit default
	var mergeMethod types.PullRequestMergeType
	if gerritsource.IsGerritOrg(identifier) {
		mergeMethod = types.MergeIfNecessary
	} else {
		mergeMethod = c.Tide.MergeMethod(orgRepo)
	}

	log.WithField("merge-strategy", mergeMethod).Debug("Using merge strategy.")
	if err := ensureCommits(repo, baseSHA, headSHAs...); err != nil {
		return nil, fmt.Errorf("failed to fetch headSHAs: %v", err)
	}
	if err := repo.MergeAndCheckout(baseSHA, string(mergeMethod), headSHAs...); err != nil {
		return nil, fmt.Errorf("failed to merge: %w", err)
	}

	return ReadProwYAML(log, repo.Directory(), false)
}

func ensureCommits(repo git.RepoClient, baseSHA string, headSHAs ...string) error {
	//Ensure baseSHA exists.
	if exists, _ := repo.CommitExists(baseSHA); !exists {
		if err := repo.Fetch(baseSHA); err != nil {
			return fmt.Errorf("failed to fetch baseSHA: %s: %v", baseSHA, err)
		}
	}
	//Ensure headSHAs exist
	for _, sha := range headSHAs {
		// This is best effort. No need to check for error
		if exists, _ := repo.CommitExists(sha); !exists {
			if err := repo.Fetch(sha); err != nil {
				return fmt.Errorf("failed to fetch headSHA: %s: %v", sha, err)
			}
		}
	}
	return nil
}

// ReadProwYAML parses the .prow.yaml file or .prow directory, no commit checkout or defaulting is included.
func ReadProwYAML(log *logrus.Entry, dir string, strict bool) (*ProwYAML, error) {
	prowYAML := &ProwYAML{}
	var opts []yaml.JSONOpt
	if strict {
		opts = append(opts, yaml.DisallowUnknownFields)
	}

	prowYAMLDirPath := path.Join(dir, inRepoConfigDirName)
	log.Debugf("Attempting to read config files under %q.", prowYAMLDirPath)
	if fileInfo, err := os.Stat(prowYAMLDirPath); !os.IsNotExist(err) && err == nil && fileInfo.IsDir() {
		mergeProwYAML := func(a, b *ProwYAML) *ProwYAML {
			c := &ProwYAML{}
			c.Presets = append(a.Presets, b.Presets...)
			c.Presubmits = append(a.Presubmits, b.Presubmits...)
			c.Postsubmits = append(a.Postsubmits, b.Postsubmits...)

			return c
		}
		prowIgnore, err := gitignore.NewRepositoryWithFile(dir, ProwIgnoreFileName)
		if err != nil {
			return nil, fmt.Errorf("failed to create `%s` parser: %w", ProwIgnoreFileName, err)
		}
		err = filepath.Walk(prowYAMLDirPath, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && (filepath.Ext(p) == ".yaml" || filepath.Ext(p) == ".yml") {
				// Use 'Match' directly because 'Ignore' and 'Include' don't work properly for repositories.
				match := prowIgnore.Match(p)
				if match != nil && match.Ignore() {
					return nil
				}
				log.Debugf("Reading YAML file %q", p)
				bytes, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				partialProwYAML := &ProwYAML{}
				if err := yaml.Unmarshal(bytes, partialProwYAML, opts...); err != nil {
					return fmt.Errorf("failed to unmarshal %q: %w", p, err)
				}
				prowYAML = mergeProwYAML(prowYAML, partialProwYAML)
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("failed to read contents of directory %q: %w", inRepoConfigDirName, err)
		}
	} else {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading %q: %w", prowYAMLDirPath, err)
		}
		log.WithField("file", inRepoConfigFileName).Debug("Attempting to get inreconfigfile")
		prowYAMLFilePath := path.Join(dir, inRepoConfigFileName)
		if _, err := os.Stat(prowYAMLFilePath); err == nil {
			bytes, err := os.ReadFile(prowYAMLFilePath)
			if err != nil {
				return nil, fmt.Errorf("failed to read %q: %w", prowYAMLDirPath, err)
			}
			if err := yaml.Unmarshal(bytes, prowYAML, opts...); err != nil {
				return nil, fmt.Errorf("failed to unmarshal %q: %w", prowYAMLDirPath, err)
			}
		} else {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to check if file %q exists: %w", prowYAMLDirPath, err)
			}
		}
	}
	return prowYAML, nil
}

// prowYAMLGetterWithDefaults is like prowYAMLGetter, but additionally sets
// defaults by calling DefaultAndValidateProwYAML.
func prowYAMLGetterWithDefaults(
	c *Config,
	gc git.ClientFactory,
	identifier string,
	baseSHA string,
	headSHAs ...string) (*ProwYAML, error) {

	prowYAML, err := prowYAMLGetter(c, gc, identifier, baseSHA, headSHAs...)
	if err != nil {
		return nil, err
	}

	// Mutate prowYAML to default values as necessary.
	if err := DefaultAndValidateProwYAML(c, prowYAML, identifier); err != nil {
		return nil, err
	}

	return prowYAML, nil
}

func DefaultAndValidateProwYAML(c *Config, p *ProwYAML, identifier string) error {
	if err := defaultPresubmits(p.Presubmits, p.Presets, c, identifier); err != nil {
		return err
	}
	if err := defaultPostsubmits(p.Postsubmits, p.Presets, c, identifier); err != nil {
		return err
	}
	if err := c.validatePresubmits(append(p.Presubmits, c.GetPresubmitsStatic(identifier)...)); err != nil {
		return err
	}
	if err := c.validatePostsubmits(append(p.Postsubmits, c.GetPostsubmitsStatic(identifier)...)); err != nil {
		return err
	}

	var errs []error
	for _, pre := range p.Presubmits {
		if !c.InRepoConfigAllowsCluster(pre.Cluster, identifier) {
			errs = append(errs, fmt.Errorf("cluster %q is not allowed for repository %q", pre.Cluster, identifier))
		}
	}
	for _, post := range p.Postsubmits {
		if !c.InRepoConfigAllowsCluster(post.Cluster, identifier) {
			errs = append(errs, fmt.Errorf("cluster %q is not allowed for repository %q", post.Cluster, identifier))
		}
	}

	if len(errs) == 0 {
		log := logrus.WithField("repo", identifier)
		log.Debugf("Successfully got %d presubmits and %d postsubmits.", len(p.Presubmits), len(p.Postsubmits))
	}

	return utilerrors.NewAggregate(errs)
}

// InRepoConfigGitCache is a wrapper around a git.ClientFactory that allows for
// threadsafe reuse of git.RepoClients when one already exists for the specified repo.
type InRepoConfigGitCache struct {
	git.ClientFactory
	cache map[string]*skipCleanRepoClient
	sync.RWMutex
}

func NewInRepoConfigGitCache(factory git.ClientFactory) git.ClientFactory {
	if factory == nil {
		// Don't wrap a nil git factory, keep it nil so that errors are handled properly.
		return nil
	}
	return &InRepoConfigGitCache{
		ClientFactory: factory,
		cache:         map[string]*skipCleanRepoClient{},
	}
}

func (c *InRepoConfigGitCache) ClientFor(org, repo string) (git.RepoClient, error) {
	key := fmt.Sprintf("%s/%s", org, repo)
	getCache := func(threadSafe bool) (git.RepoClient, error) {
		if client, ok := c.cache[key]; ok {
			client.Lock()
			// if repo is dirty, perform git reset --hard instead of deleting entire repo
			if isDirty, err := client.RepoClient.IsDirty(); err != nil || isDirty {
				if err := client.ResetHard("HEAD"); err != nil {
					if threadSafe {
						// Called within client `Lock`, safe to delete from map,
						// return with nil so that a fresh clone will be performed
						delete(c.cache, key)
						client.Clean() // best effort clean, to avoid jam up disk
					}
					// Called with client `RLock`, not safe to delete from map,
					// also return because fetch doesn't make much sense any more
					client.Unlock()
					return nil, nil
				}
			}
			// Don't unlock the client unless we get an error or the consumer
			// indicates they are done by Clean()ing.
			// This fetch operation is repeated executed in the clone repo,
			// which fails if there is a commit being deleted from remote. This
			// is a corner case, but when it happens it would be really
			// annoying, adding `--prune` tag here for mitigation.
			if err := client.Fetch("--prune"); err != nil {
				client.Unlock()
				return nil, err
			}
			return client, nil
		}
		return nil, nil
	}
	c.RLock()
	cached, err := getCache(false)
	c.RUnlock()
	if cached != nil || err != nil {
		return cached, err
	}

	// The repo client was not cached, create a new one.
	c.Lock()
	defer c.Unlock()
	// On cold start, all threads pass RLock and wait here, we need to do one more
	// check here to avoid more than one cloning.
	// (It would be nice if we could upgrade from `RLock` to `Lock`)
	cached, err = getCache(true)
	if cached != nil || err != nil {
		return cached, err
	}
	coreClient, err := c.ClientFactory.ClientForWithRepoOpts(org, repo, inrepoconfigRepoOpts)
	if err != nil {
		return nil, err
	}
	// This is the easiest way we can find for fetching all pull heads
	if err := coreClient.Config("--add", "remote.origin.fetch", "+refs/pull/*/head:refs/remotes/origin/pr/*"); err != nil {
		return nil, err
	}
	client := &skipCleanRepoClient{
		RepoClient: coreClient,
	}
	client.Lock()
	c.cache[key] = client
	return client, nil
}

var _ git.RepoClient = &skipCleanRepoClient{}

type skipCleanRepoClient struct {
	git.RepoClient
	sync.Mutex
}

func (rc *skipCleanRepoClient) Clean() error {
	// Skip cleaning and unlock to allow reuse as a cached entry.
	rc.Mutex.Unlock()
	return nil
}

// ContainsInRepoConfigPath indicates whether the specified list of changed
// files (repo relative paths) includes a file that might be an inrepo config file.
//
// This function could report a false positive as it doesn't consider .prowignore files.
// It is designed to be used to help short circuit when we know a change doesn't touch
// the inrepo config.
func ContainsInRepoConfigPath(files []string) bool {
	for _, file := range files {
		if file == inRepoConfigFileName {
			return true
		}
		if strings.HasPrefix(file, inRepoConfigDirName+"/") {
			return true
		}
	}
	return false
}
