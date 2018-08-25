package validator

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/google/go-github/github"
	"github.com/pkg/errors"
)

// Context contains an event payload an a configured client
type Context struct {
	Event  interface{}
	Github *github.Client
	Ctx    *context.Context
}

// Process handles webhook events kinda like Probot does
func (c *Context) Process() {
	switch e := c.Event.(type) {
	case *github.CheckSuiteEvent:
		c.ProcessCheckSuite(c.Event.(*github.CheckSuiteEvent))
		return
	case *github.PullRequestEvent:
		prEvent := c.Event.(*github.PullRequestEvent)
		if *prEvent.Action == "opened" {
			_, err := c.Github.Checks.RequestCheckSuite(*c.Ctx, e.Repo.GetOwner().GetLogin(), e.Repo.GetName(), github.RequestCheckSuiteOptions{
				HeadSHA: prEvent.GetPullRequest().GetHead().GetSHA(),
			})
			if err != nil {
				log.Printf("%+v\n", err)
			}
		}
	default:
		log.Printf("ignoring %s\n", reflect.TypeOf(e).String())
		return
	}
}

// ProcessCheckSuite validates the Kubernetes YAML that has changed on checks
// associated with PRs.
func (c *Context) ProcessCheckSuite(e *github.CheckSuiteEvent) {
	if *e.Action == "created" || *e.Action == "requested" || *e.Action == "rerequested" {
		createCheckRunErr := c.createInitialCheckRun(e)
		if createCheckRunErr != nil {
			// TODO return a 500 to signal that retry is preferred
			log.Println(errors.Wrap(createCheckRunErr, "Couldn't create check run"))
			return
		}

		checkRunStart := time.Now()
		var annotations []*github.CheckRunAnnotation

		config, configAnnotation, err := c.kubeValidatorConfigOrAnnotation(e)
		if err != nil {
			c.createConfigMissingCheckRun(&checkRunStart, e)
			return
		}
		if configAnnotation != nil {
			annotations = append(annotations, configAnnotation)
		}

		// Determine which files to validate
		changedFileList, fileListError := c.changedFileList(e)
		if fileListError != nil {
			// TODO fail the checkrun instead
			log.Println(fileListError)
			return
		}

		filesToValidate := config.matchingCandidates(changedFileList)

		// Validate the files
		for filename, file := range filesToValidate {
			bytes, err := c.bytesForFilename(e, filename)
			if err != nil {
				annotations = append(annotations, &github.CheckRunAnnotation{
					FileName:     file.File.Filename,
					BlobHRef:     file.File.BlobURL,
					StartLine:    github.Int(1),
					EndLine:      github.Int(1),
					WarningLevel: github.String("failure"),
					Title:        github.String(fmt.Sprintf("Error loading %s from GitHub", *file.File.Filename)),
					Message:      github.String(fmt.Sprintf("%+v", err)),
				})
			}

			if file.Schemas == nil {
				fileAnnotations := AnnotateFile(bytes, file.File)
				annotations = append(annotations, fileAnnotations...)
			}

			for _, schema := range file.Schemas {
				fileAnnotations := AnnotateFileWithSchema(bytes, file.File, schema)
				annotations = append(annotations, fileAnnotations...)
			}

		}

		// Annotate the PR
		finalCheckRunErr := c.createFinalCheckRun(&checkRunStart, e, filesToValidate, annotations)
		if finalCheckRunErr != nil {
			// TODO return a 500 to signal that retry is preferred
			log.Println(errors.Wrap(finalCheckRunErr, "Couldn't create check run"))
			return
		}
	}
	return
}
