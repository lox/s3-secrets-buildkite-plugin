package secrets

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"strings"
)

// Client represents interaction with AWS S3
type Client interface {
	Get(bucket, key string) ([]byte, error)
	BucketExists(bucket string) (bool, error)
}

// Agent represents interaction with an ssh-agent process
type Agent interface {
	Add(key []byte) error
	Pid() uint
}

// Config holds all the parameters for Run()
type Config struct {
	// Repo from BUILDKITE_REPO
	Repo string

	// Bucket from BUILDKITE_PLUGIN_S3_SECRETS_BUCKET
	Bucket string

	// Prefix within bucket, from BUILDKITE_PLUGIN_S3_SECRETS_BUCKET_PREFIX,
	// defaulting to the value of BUILDKITE_PIPELINE_SLUG
	Prefix string

	// Client for S3
	Client Client

	// Logger is expected to output to stderr
	Logger *log.Logger

	// SSHAgent represents an ssh-agent process
	SSHAgent Agent

	// EnvSink has the contents of environment files written to it
	EnvSink io.Writer

	// GitCredentialHelper is the path to git-credential-s3-secrets
	GitCredentialHelper string
}

// Run is the programmatic (as opposed to CLI) entrypoint to all
// functionality; secrets are downloaded from S3, and loaded into ssh-agent
// etc.
func Run(conf Config) error {
	bucket := conf.Bucket
	log := conf.Logger

	log.Printf("~~~ Downloading secrets from :s3: %s", bucket)

	if ok, err := conf.Client.BucketExists(bucket); !ok {
		log.Printf("+++ :warning: Bucket %q doesn't exist", bucket)
		if err != nil {
			log.Println(err)
		}
		return fmt.Errorf("bucket %q not found", bucket)
	}

	resultsSSH := make(chan getResult)
	getSSHKeys(conf, resultsSSH)

	resultsEnv := make(chan getResult)
	getEnvs(conf, resultsEnv)

	resultsGit := make(chan getResult)
	getGitCredentials(conf, resultsGit)

	if err := handleSSHKeys(conf, resultsSSH); err != nil {
		return err
	}
	if err := handleEnvs(conf, resultsEnv); err != nil {
		return err
	}
	if err := handleGitCredentials(conf, resultsGit); err != nil {
		return err
	}
	return nil
}

func getSSHKeys(conf Config, results chan<- getResult) {
	keys := []string{
		conf.Prefix + "/private_ssh_key",
		conf.Prefix + "/id_rsa_github",
		"private_ssh_key",
		"id_rsa_github",
	}
	conf.Logger.Printf("Checking S3 for SSH keys:")
	for _, k := range keys {
		conf.Logger.Printf("- %s", k)
	}
	go GetAll(conf.Client, conf.Bucket, keys, results)
}

func getEnvs(conf Config, results chan<- getResult) {
	keys := []string{
		"env",
		"environment",
		conf.Prefix + "/env",
		conf.Prefix + "/environment",
	}
	conf.Logger.Printf("Checking S3 for environment files:")
	for _, k := range keys {
		conf.Logger.Printf("- %s", k)
	}
	go GetAll(conf.Client, conf.Bucket, keys, results)
}

func getGitCredentials(conf Config, results chan<- getResult) {
	keys := []string{
		"git-credentials",
		conf.Prefix + "/git-credentials",
	}
	conf.Logger.Printf("Checking S3 for git credentials:")
	for _, k := range keys {
		conf.Logger.Printf("- %s", k)
	}
	go GetAll(conf.Client, conf.Bucket, keys, results)
}

func handleSSHKeys(conf Config, results <-chan getResult) error {
	log := conf.Logger
	keyFound := false
	for r := range results {
		if r.err != nil {
			// TODO: silently ignore NotFound & Forbidden errors
			log.Printf("+++ :warning: Failed to download ssh-key %s/%s: %v", r.bucket, r.key, r.err)
			continue
		}
		log.Printf(
			"Loading %s/%s (%d bytes) into ssh-agent (pid %d)",
			r.bucket, r.key, len(r.data), conf.SSHAgent.Pid(),
		)
		if err := conf.SSHAgent.Add(r.data); err != nil {
			return fmt.Errorf("ssh-agent add: %w", err)
		}
		keyFound = true
	}
	if !keyFound && strings.HasPrefix(conf.Repo, "git@") {
		log.Printf("+++ :warning: Failed to find an SSH key in secret bucket")
		log.Printf(
			"The repository %q appears to use SSH for transport, but the elastic-ci-stack-s3-secrets-hooks plugin did not find any SSH keys in the %q S3 bucket.",
			conf.Repo, conf.Bucket,
		)
		log.Printf("See https://github.com/buildkite/elastic-ci-stack-for-aws#build-secrets for more information.")
	}
	return nil
}

func handleEnvs(conf Config, results <-chan getResult) error {
	log := conf.Logger
	for r := range results {
		if r.err != nil {
			// TODO: silently ignore NotFound & Forbidden errors
			log.Printf("+++ :warning: Failed to download env from %s/%s: %v", r.bucket, r.key, r.err)
			continue
		}
		data := r.data
		if data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		log.Printf("Loading %s/%s (%d bytes) of env", r.bucket, r.key, len(r.data))
		// TODO: mutex on EnvSink
		if _, err := bytes.NewReader(data).WriteTo(conf.EnvSink); err != nil {
			return fmt.Errorf("copying env: %w", err)
		}
	}
	return nil
}

func handleGitCredentials(conf Config, results <-chan getResult) error {
	log := conf.Logger
	var helpers []string
	for r := range results {
		if r.err != nil {
			continue
		}
		log.Printf("Adding git-credentials in %s/%s as a credential helper", r.bucket, r.key)
		helpers = append(helpers, fmt.Sprintf(
			"'credential.helper=%s %s %s'",
			conf.GitCredentialHelper, r.bucket, r.key,
		))
	}
	if len(helpers) == 0 {
		return nil
	}
	env := "GIT_CONFIG_PARAMETERS=" + strings.Join(helpers, " ") + "\n"
	// TODO: mutex on EnvSink
	if _, err := io.WriteString(conf.EnvSink, env); err != nil {
		return fmt.Errorf("writing GIT_CONFIG_PARAMETERS env: %w", err)
	}
	return nil
}

type getResult struct {
	bucket string
	key    string
	data   []byte
	err    error
}

// GetAll fetches keys from an S3 bucket concurrently.
// Concurrency is unbounded; intended for use with a handful of keys.
// Results are sent to a channel in the originally requested order.
// This is done by creating a chain of channels between each goroutine.
// The results channel is passed through that chain.
func GetAll(c Client, bucket string, keys []string, results chan<- getResult) {
	// first link in chain; will pass results channel into the first goroutine
	link := make(chan chan<- getResult, 1)
	link <- results
	close(link)

	for _, k := range keys {
		// next link in chain; will pass results channel to the next goroutine.
		nextLink := make(chan chan<- getResult)

		// goroutine immediately fetches from S3, then waits for its turn to send
		// to the results channel; concurrent fetch, ordered results.
		go func(k string, link <-chan chan<- getResult, nextLink chan<- chan<- getResult) {
			data, err := c.Get(bucket, k)
			results := <-link // wait for results channel from previous goroutine
			results <- getResult{bucket: bucket, key: k, data: data, err: err}
			nextLink <- results // send results channel to the next goroutine
			close(nextLink)
		}(k, link, nextLink)

		link = nextLink // our `nextLink` becomes `link` for the next goroutine.
	}
	close(<-link) // wait for final goroutine, close results channel
}
