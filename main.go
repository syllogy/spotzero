package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/doitintl/spot-asg/internal/aws/eventbridge"

	"github.com/doitintl/spot-asg/internal/aws/autoscaling"
	"github.com/doitintl/spot-asg/internal/aws/sts"
	"github.com/urfave/cli/v2"

	"github.com/aws/aws-lambda-go/lambda"
)

var (
	// main context
	mainCtx context.Context
	// Version contains the current version.
	Version = "dev"
	// BuildDate contains a string with the build date.
	BuildDate = "unknown"
	// GitCommit build git commit SHA
	GitCommit = "dirty"
	// GitBranch build git branch
	GitBranch = "master"
	// app name
	appName = "spot-asg"
	// lambda mode
	lambdaMode bool
	// IAM Role to scan ASG groups
	role sts.AssumeRoleInRegion
	// IAM Role to put events into Event Bus
	ebRole sts.AssumeRoleInRegion
	// event bus ARN
	eventBusArn string
)

func parseTags(list []string) map[string]string {
	tags := make(map[string]string, len(list))
	for _, t := range list {
		kv := strings.Split(t, "=")
		if len(kv) == 2 {
			tags[kv[0]] = kv[1]
		}
	}
	return tags
}

// handle Linux innteruption signals
func handleSignals() context.Context {
	// Graceful shut-down on SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// create cancelable context
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		defer cancel()
		sid := <-sig
		log.Printf("received signal: %d\n", sid)
		log.Println("canceling main command ...")
	}()

	return ctx
}

func init() {
	// handle termination signal
	mainCtx = handleSignals()
}

func getCallerIdentity(role sts.AssumeRoleInRegion) error {
	checker := sts.NewRoleChecker(role)
	result, err := checker.CheckRole(mainCtx)
	if err != nil {
		return err
	}
	log.Print(result)
	return nil
}

func listAutoscalingGroups(asgRole, ebRole sts.AssumeRoleInRegion, eventBusArn string, tags map[string]string) error {
	lister := autoscaling.NewAsgLister(asgRole)
	groups, err := lister.ListGroups(mainCtx, tags)
	if err != nil {
		return err
	}
	if eventBusArn != "" {
		publisher := eventbridge.NewAsgPublisher(ebRole, eventBusArn)
		events := make([]interface{}, len(groups))
		for i, v := range groups {
			events[i] = v
		}
		err := publisher.PublishEvents(mainCtx, events)
		if err != nil {
			return err
		}
	} else {
		log.Print(groups)
	}
	return nil
}

func updateAutoscalingGroups(role sts.AssumeRoleInRegion, tags map[string]string) error {
	lister := autoscaling.NewAsgLister(role)
	updater := autoscaling.NewAsgUpdater(role)
	// get list of ASG groups filtered by tags
	groups, err := lister.ListGroups(mainCtx, tags)
	if err != nil {
		return err
	}
	// update ASG groups one by one; skip on error (log only)
	var updateError error // keep last update error
	for _, group := range groups {
		log.Printf("update autoscaling group %v", *group.AutoScalingGroupARN)
		err = updater.UpdateAutoScalingGroup(mainCtx, group)
		if err != nil {
			// report error to log and try to update other groups
			log.Printf("failed to update autoscaling group %v", *group.AutoScalingGroupARN)
			updateError = err
		}
	}
	return updateError
}

func recommendAutoscalingGroups(role sts.AssumeRoleInRegion, tags map[string]string) error {
	lister := autoscaling.NewAsgLister(role)
	updater := autoscaling.NewAsgUpdater(role)
	// get list of ASG groups filtered by tags
	groups, err := lister.ListGroups(mainCtx, tags)
	if err != nil {
		return err
	}
	var publisher eventbridge.AsgPublisher
	if eventBusArn != "" {
		publisher = eventbridge.NewAsgPublisher(ebRole, eventBusArn)
	}
	// recommend optimization for ASG groups one by one; skip on error (log only)
	var recommendError error // keep last update error
	for _, group := range groups {
		log.Printf("get recommedation for autoscaling group %v", *group.AutoScalingGroupARN)
		input, err := updater.CreateAutoScalingGroupUpdateInput(mainCtx, group)
		if err != nil {
			// report error to log and try to update other groups
			log.Printf("failed to recommend optimization for autoscaling group %v", *group.AutoScalingGroupARN)
			recommendError = err
		}
		if publisher != nil {
			err := publisher.PublishEvents(mainCtx, []interface{}{input})
			if err != nil {
				return err
			}
		} else {
			log.Print(input)
		}
	}
	return recommendError
}

// =========== CLI Commands ===========

func getCallerIdentityCmd(c *cli.Context) error {
	log.Printf("getting AWS caller identity with %s", c.FlagNames())
	if lambdaMode {
		lambda.StartWithContext(mainCtx, func(ctx context.Context) error {
			return getCallerIdentity(role)
		})
		return nil
	}
	return getCallerIdentity(role)
}

// =========== List ASG groups Handlers ===========

func listAutoscalingGroupsCmd(c *cli.Context) error {
	tags := parseTags(c.StringSlice("tags"))
	log.Printf("get autoscaling groups filtered by %v", tags)
	// handle lambda or cli
	if lambdaMode {
		lambda.StartWithContext(mainCtx, func(ctx context.Context) error {
			return listAutoscalingGroups(role, ebRole, eventBusArn, tags)
		})
		return nil
	}
	return listAutoscalingGroups(role, ebRole, eventBusArn, tags)
}

// =========== Update ASG groups Handlers ===========

func updateAutoscalingGroupsCmd(c *cli.Context) error {
	tags := parseTags(c.StringSlice("tags"))
	log.Printf("update autoscaling groups filtered by %v", tags)
	// handle lambda or cli
	if lambdaMode {
		lambda.StartWithContext(mainCtx, func(ctx context.Context) error {
			return updateAutoscalingGroups(role, tags)
		})
		return nil
	}
	return updateAutoscalingGroups(role, tags)
}

// =========== Update ASG groups Handlers ===========

func recommendAutoscalingGroupsCmd(c *cli.Context) error {
	tags := parseTags(c.StringSlice("tags"))
	log.Printf("recommend optimization for autoscaling groups filtered by %v", tags)
	// handle lambda or cli
	if lambdaMode {
		lambda.StartWithContext(mainCtx, func(ctx context.Context) error {
			return recommendAutoscalingGroups(role, tags)
		})
		return nil
	}
	return recommendAutoscalingGroups(role, tags)
}

// =========== MAIN ===========

func main() {
	// shared flags: list and spotize command
	sharedFlags := []cli.Flag{
		&cli.StringSliceFlag{
			Name:  "tags",
			Usage: "tags to filter by (syntax: key=value)",
		},
		&cli.StringFlag{
			Name:        "eb-eventbus-arn",
			Usage:       "send list output to the specified Amazon EventBrige Event Bus",
			Destination: &eventBusArn,
		},
		&cli.StringFlag{
			Name:        "eb-role-arn",
			Usage:       "role ARN to assume for sending events to the Event Bus",
			Destination: &ebRole.Arn,
		},
		&cli.StringFlag{
			Name:        "eb-external-id",
			Usage:       "external ID to assume role with",
			Destination: &ebRole.ExternalID,
		},
		&cli.StringFlag{
			Name:        "eb-region",
			Usage:       "the AWS Region of EventBridge Event Bus",
			Destination: &ebRole.Region,
		},
	}
	// main app
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "lambda-mode",
				Usage:       "set to true if running as AWS Lambda",
				Destination: &lambdaMode,
			},
			&cli.StringFlag{
				Name:        "role-arn",
				Usage:       "role ARN to assume",
				Destination: &role.Arn,
			},
			&cli.StringFlag{
				Name:        "external-id",
				Usage:       "external ID to assume role with",
				Destination: &role.ExternalID,
			},
			&cli.StringFlag{
				Name:        "region",
				Usage:       "the AWS Region to send the request to",
				Destination: &role.Region,
			},
		},
		Commands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "list EC2 autoscaling groups, filtered by tags",
				Action: listAutoscalingGroupsCmd,
				Flags:  sharedFlags,
			},
			{
				Name:   "update",
				Usage:  "update EC2 autoscaling groups to maximize Spot usage",
				Action: updateAutoscalingGroupsCmd,
				Flags: []cli.Flag{
					&cli.StringSliceFlag{
						Name:  "tags",
						Usage: "tags to filter by (syntax: key=value)",
					},
				},
			},
			{
				Name:   "recommend",
				Usage:  "recommend optimization for EC2 autoscaling groups to maximize Spot usage",
				Action: recommendAutoscalingGroupsCmd,
				Flags:  sharedFlags,
			},
			{
				Name:   "get-caller-identity",
				Usage:  "get AWS caller identity",
				Action: getCallerIdentityCmd,
			},
		},
		Name:    appName,
		Usage:   "update/create MixedInstancePolicy for Amazon EC2 AutoScaling groups",
		Version: Version,
	}
	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Printf("%s %s\n", appName, Version)
		fmt.Printf("  Build date: %s\n", BuildDate)
		fmt.Printf("  Git commit: %s\n", GitCommit)
		fmt.Printf("  Git branch: %s\n", GitBranch)
		fmt.Printf("  Built with: %s\n", runtime.Version())
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
