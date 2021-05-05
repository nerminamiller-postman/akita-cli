package upload

import (
	"context"
	"fmt"
	"io/ioutil"

	"github.com/logrusorgru/aurora"
	"github.com/pkg/errors"

	"github.com/akitasoftware/akita-libs/akid"
	"github.com/akitasoftware/akita-libs/akiuri"
	"github.com/akitasoftware/akita-libs/api_schema"

	"github.com/akitasoftware/akita-cli/apispec"
	"github.com/akitasoftware/akita-cli/printer"
	"github.com/akitasoftware/akita-cli/rest"
	"github.com/akitasoftware/akita-cli/trace"
	"github.com/akitasoftware/akita-cli/util"
)

func Run(args Args) error {
	// Resolve ServiceID
	frontClient := rest.NewFrontClient(args.Domain, args.ClientID)
	svc, err := util.GetServiceIDByName(frontClient, args.DestURI.ServiceName)
	if err != nil {
		return errors.Wrapf(err, "failed to resolve service name %q", args.DestURI.ServiceName)
	}

	// Determine the object's name.
	objectName := args.DestURI.ObjectName
	if objectName == "" {
		switch *args.DestURI.ObjectType {
		case akiuri.SPEC:
			objectName = util.RandomAPIModelName()
		case akiuri.TRACE:
			objectName = util.RandomLearnSessionName()
		default:
			return errors.Errorf("unknown object type: %q", args.DestURI.ObjectType)
		}
	}

	// Do the upload.
	learnClient := rest.NewLearnClient(args.Domain, args.ClientID, svc)
	switch *args.DestURI.ObjectType {
	case akiuri.SPEC:
		if err := uploadSpec(learnClient, args, objectName); err != nil {
			return err
		}

	case akiuri.TRACE:
		if err := uploadTraces(learnClient, args, svc, objectName); err != nil {
			return err
		}

	default:
		return errors.Errorf("unknown object type: %q", args.DestURI.ObjectType)
	}

	// Display the resulting URI to the user.
	uri := akiuri.URI{
		ServiceName: args.DestURI.ServiceName,
		ObjectType:  args.DestURI.ObjectType,
		ObjectName:  objectName,
	}
	printer.Stderr.Infof("%s 🎉\n", aurora.Green("Success!"))
	printer.Stderr.Infof("Your upload is available as: ")
	fmt.Println(uri.String()) // print URI to stdout for easy scripting

	return nil
}

func uploadSpec(learnClient rest.LearnClient, args Args, specName string) error {
	// Read file content.
	fileContent, err := ioutil.ReadFile(args.FilePaths[0])
	if err != nil {
		return errors.Wrapf(err, "failed to read %q", args.FilePaths[0])
	}

	printer.Stderr.Infof("Uploading...\n")
	req := api_schema.UploadSpecRequest{
		Name:    specName,
		Content: string(fileContent),
	}
	ctx, cancel := context.WithTimeout(context.Background(), args.UploadTimeout)
	defer cancel()
	if _, err := learnClient.UploadSpec(ctx, req); err != nil {
		return errors.Wrap(err, "upload failed")
	}

	return nil
}

func uploadTraces(learnClient rest.LearnClient, args Args, serviceID akid.ServiceID, traceName string) error {
	// Attempt to get the trace ID. First, see if the trace already exists.
	traceID, err := util.GetLearnSessionIDByName(learnClient, traceName)
	if err != nil {
		// XXX Assume the error means that the session doesn't already exist. We
		// XXX should check this assumption, but errors appear to be too opaque to
		// XXX do this easily.

		// If we are supposed to be appending to an existing trace, warn that the
		// trace doesn't yet exist.
		if args.Append {
			printer.Stderr.Warningf("trace %q doesn't yet exist; creating it\n", traceName)
		}

		// Attempt to create the trace.
		printer.Stderr.Infof("Creating trace...\n")
		tags := map[string]string{}
		traceID, err = util.NewLearnSession(args.Domain, args.ClientID, serviceID, traceName, tags, nil)
		if err != nil {
			return errors.Wrapf(err, "failed to create trace %q", traceName)
		}
	} else if !args.Append {
		// The trace already exists, but the user has not asked to append to it. Cowardly avoid accidentally modifying the trace.
		return errors.Errorf("trace %q already exists. Use \"--append\" if you wish to add events to the trace", traceName)
	}

	inboundCount := trace.NewPacketCountSummary()
	outboundCount := trace.NewPacketCountSummary()

	// Create collector for ingesting the trace events.
	inboundCollector := trace.NewBackendCollector(serviceID, traceID, learnClient, api_schema.Inbound, args.Plugins, inboundCount)
	outboundCollector := trace.NewBackendCollector(serviceID, traceID, learnClient, api_schema.Outbound, args.Plugins, outboundCount)
	defer inboundCollector.Close()
	defer outboundCollector.Close()

	if !args.IncludeTrackers {
		inboundCollector = trace.New3PTrackerFilterCollector(inboundCollector)
		outboundCollector = trace.New3PTrackerFilterCollector(outboundCollector)
	}

	for _, harFileName := range args.FilePaths {
		printer.Stderr.Infof("Uploading %q...\n", harFileName)
		if err := apispec.ProcessHAR(inboundCollector, outboundCollector, harFileName); err != nil {
			return errors.Wrapf(err, "failed to process HAR file %q", harFileName)
		}
	}

	// Outbound is only used if the HAR file has an Akita extension to mark it as such.
	totalRequests := inboundCount.Total().HTTPRequests + outboundCount.Total().HTTPRequests
	totalResponses := inboundCount.Total().HTTPResponses + outboundCount.Total().HTTPResponses

	// This is not entirely true because the last 0-9 won't be updated until
	// Closed() is called, but we don't have stats on the upload portion.
	printer.Stderr.Infof("Uploaded %d requests and %d responses.\n", totalRequests, totalResponses)

	return nil
}