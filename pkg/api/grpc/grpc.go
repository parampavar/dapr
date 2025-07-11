/*
Copyright 2021 The Dapr Authors
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

package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	otelbaggage "go.opentelemetry.io/otel/baggage"
	otelTrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcMetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/dapr/components-contrib/bindings"
	"github.com/dapr/components-contrib/configuration"
	"github.com/dapr/components-contrib/contenttype"
	contribMetadata "github.com/dapr/components-contrib/metadata"
	"github.com/dapr/components-contrib/pubsub"
	"github.com/dapr/components-contrib/state"
	actorapi "github.com/dapr/dapr/pkg/actors/api"
	actorerrors "github.com/dapr/dapr/pkg/actors/errors"
	apierrors "github.com/dapr/dapr/pkg/api/errors"
	"github.com/dapr/dapr/pkg/api/grpc/metadata"
	"github.com/dapr/dapr/pkg/api/universal"
	stateLoader "github.com/dapr/dapr/pkg/components/state"
	"github.com/dapr/dapr/pkg/config"
	diag "github.com/dapr/dapr/pkg/diagnostics"
	diagConsts "github.com/dapr/dapr/pkg/diagnostics/consts"
	diagUtils "github.com/dapr/dapr/pkg/diagnostics/utils"
	"github.com/dapr/dapr/pkg/encryption"
	"github.com/dapr/dapr/pkg/messages"
	"github.com/dapr/dapr/pkg/messages/errorcodes"
	invokev1 "github.com/dapr/dapr/pkg/messaging/v1"
	"github.com/dapr/dapr/pkg/outbox"
	commonv1pb "github.com/dapr/dapr/pkg/proto/common/v1"
	internalv1pb "github.com/dapr/dapr/pkg/proto/internals/v1"
	runtimev1pb "github.com/dapr/dapr/pkg/proto/runtime/v1"
	"github.com/dapr/dapr/pkg/resiliency"
	"github.com/dapr/dapr/pkg/resiliency/breaker"
	"github.com/dapr/dapr/pkg/runtime/channels"
	"github.com/dapr/dapr/pkg/runtime/processor"
	runtimePubsub "github.com/dapr/dapr/pkg/runtime/pubsub"
	"github.com/dapr/dapr/utils"
	kiterrors "github.com/dapr/kit/errors"
	"github.com/dapr/kit/logger"
)

const (
	daprHTTPStatusHeader = "dapr-http-status"
	metadataPrefix       = "metadata."
)

// API is the gRPC interface for the Dapr gRPC API. It implements both the internal and external proto definitions.
type API interface {
	io.Closer

	// DaprInternal Service methods
	internalv1pb.ServiceInvocationServer

	// Dapr Service methods
	runtimev1pb.DaprServer
}

type api struct {
	*universal.Universal
	logger                logger.Logger
	directMessaging       invokev1.DirectMessaging
	channels              *channels.Channels
	pubsubAdapter         runtimePubsub.Adapter
	pubsubAdapterStreamer runtimePubsub.AdapterStreamer
	outbox                outbox.Outbox
	sendToOutputBindingFn func(ctx context.Context, name string, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error)
	tracingSpec           config.TracingSpec
	accessControlList     *config.AccessControlList
	processor             *processor.Processor
	wg                    sync.WaitGroup

	closeCh chan struct{}
	closed  atomic.Bool
}

// APIOpts contains options for NewAPI.
type APIOpts struct {
	Universal             *universal.Universal
	Logger                logger.Logger
	Channels              *channels.Channels
	PubSubAdapter         runtimePubsub.Adapter
	PubSubAdapterStreamer runtimePubsub.AdapterStreamer
	Outbox                outbox.Outbox
	DirectMessaging       invokev1.DirectMessaging
	SendToOutputBindingFn func(ctx context.Context, name string, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error)
	TracingSpec           config.TracingSpec
	AccessControlList     *config.AccessControlList
	Processor             *processor.Processor
}

// NewAPI returns a new gRPC API.
func NewAPI(opts APIOpts) API {
	return &api{
		Universal:             opts.Universal,
		logger:                opts.Logger,
		directMessaging:       opts.DirectMessaging,
		channels:              opts.Channels,
		pubsubAdapter:         opts.PubSubAdapter,
		pubsubAdapterStreamer: opts.PubSubAdapterStreamer,
		outbox:                opts.Outbox,
		sendToOutputBindingFn: opts.SendToOutputBindingFn,
		tracingSpec:           opts.TracingSpec,
		accessControlList:     opts.AccessControlList,
		processor:             opts.Processor,
		closeCh:               make(chan struct{}),
	}
}

// validateAndGetPubsubAndTopic validates the request parameters and returns the pubsub interface, pubsub name, topic name, rawPayload metadata if set
// or an error.
func (a *api) validateAndGetPubsubAndTopic(pubsubName, topic string, reqMeta map[string]string) (pubsub.PubSub, string, string, bool, error) {
	var err error
	if a.pubsubAdapter == nil {
		err = apierrors.PubSub(pubsubName).WithMetadata(nil).NotConfigured()
		return nil, "", "", false, err
	}

	if pubsubName == "" {
		err = apierrors.PubSub(pubsubName).WithMetadata(reqMeta).NameEmpty()
		return nil, "", "", false, err
	}

	thepubsub, ok := a.Universal.CompStore().GetPubSub(pubsubName)
	if !ok {
		err = apierrors.PubSub(pubsubName).WithMetadata(nil).NotFound()
		return nil, "", "", false, err
	}

	if topic == "" {
		err = apierrors.PubSub(pubsubName).WithMetadata(reqMeta).TopicEmpty()
		return nil, "", "", false, err
	}

	rawPayload, metaErr := contribMetadata.IsRawPayload(reqMeta)
	if metaErr != nil {
		err = apierrors.PubSub(pubsubName).WithMetadata(reqMeta).DeserializeError(metaErr)
		return nil, "", "", false, err
	}

	return thepubsub.Component, pubsubName, topic, rawPayload, nil
}

func (a *api) PublishEvent(ctx context.Context, in *runtimev1pb.PublishEventRequest) (*emptypb.Empty, error) {
	thepubsub, pubsubName, topic, rawPayload, validationErr := a.validateAndGetPubsubAndTopic(in.GetPubsubName(), in.GetTopic(), in.GetMetadata())
	if validationErr != nil {
		apiServerLogger.Debug(validationErr)
		return &emptypb.Empty{}, validationErr
	}

	body := []byte{}
	if in.GetData() != nil {
		body = in.GetData()
	}

	data := body

	if !rawPayload {
		span := diagUtils.SpanFromContext(ctx)
		traceID, traceState := diag.TraceIDAndStateFromSpan(span)

		envelope, err := runtimePubsub.NewCloudEvent(&runtimePubsub.CloudEvent{
			Source:          a.Universal.AppID(),
			Topic:           in.GetTopic(),
			DataContentType: in.GetDataContentType(),
			Data:            body,
			TraceID:         traceID,
			TraceState:      traceState,
			Pubsub:          in.GetPubsubName(),
		}, in.GetMetadata())
		if err != nil {
			nerr := apierrors.PubSub(pubsubName).WithAppError(
				a.AppID(), err,
			).CloudEventCreation()
			apiServerLogger.Debug(nerr)
			return &emptypb.Empty{}, nerr
		}

		features := thepubsub.Features()
		pubsub.ApplyMetadata(envelope, features, in.GetMetadata())

		data, err = json.Marshal(envelope)
		if err != nil {
			err = apierrors.PubSub(pubsubName).WithAppError(
				a.AppID(), nil,
			).WithTopic(topic).MarshalEnvelope()
			apiServerLogger.Debug(err)
			return &emptypb.Empty{}, err
		}
	}

	req := pubsub.PublishRequest{
		PubsubName: pubsubName,
		Topic:      topic,
		Data:       data,
		Metadata:   in.GetMetadata(),
	}

	start := time.Now()
	err := a.pubsubAdapter.Publish(ctx, &req)
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.PubsubEgressEvent(context.Background(), pubsubName, topic, err == nil, elapsed)

	if err != nil {
		var nerr error

		switch {
		case errors.As(err, &runtimePubsub.NotAllowedError{}):
			nerr = apierrors.PubSub(pubsubName).PublishForbidden(topic, a.AppID(), err)
		case errors.As(err, &runtimePubsub.NotFoundError{}):
			nerr = apierrors.PubSub(pubsubName).TestNotFound(topic, err)
		default:
			nerr = apierrors.PubSub(pubsubName).PublishMessage(topic, err)
		}

		apiServerLogger.Debug(nerr)
		return &emptypb.Empty{}, nerr
	}

	return &emptypb.Empty{}, nil
}

type invokeServiceResp struct {
	message  *commonv1pb.InvokeResponse
	headers  metadata.MD
	trailers metadata.MD
}

// These flags are used to make sure that we are printing the deprecation warning log messages in "InvokeService" just once.
// By using "CompareAndSwap(false, true)" we replace the value "false" with "true" only if it's not already "true".
// "CompareAndSwap" returns true if the swap happened (i.e. if the value was not already "true"), so we can use that as a flag to make sure we only run the code once.
// Why not using "sync.Once"? In our tests (https://github.com/dapr/dapr/pull/5740), that seems to be causing a regression in the perf tests. This is probably because when using "sync.Once" and the callback needs to be executed for the first time, all concurrent requests are blocked too. Additionally, the use of closures in this case _could_ have an impact on the GC as well.
var (
	invokeServiceDeprecationNoticeShown     = atomic.Bool{}
	invokeServiceHTTPDeprecationNoticeShown = atomic.Bool{}
)

// Deprecated: Use proxy mode service invocation instead.
func (a *api) InvokeService(ctx context.Context, in *runtimev1pb.InvokeServiceRequest) (*commonv1pb.InvokeResponse, error) {
	if a.directMessaging == nil {
		return nil, messages.ErrDirectInvokeNotReady
	}

	if invokeServiceDeprecationNoticeShown.CompareAndSwap(false, true) {
		apiServerLogger.Warn("[DEPRECATION NOTICE] InvokeService is deprecated and will be removed in the future, please use proxy mode instead.")
	}
	policyDef := a.Universal.Resiliency().EndpointPolicy(in.GetId(), in.GetId()+":"+in.GetMessage().GetMethod())

	req := invokev1.FromInvokeRequestMessage(in.GetMessage())
	if policyDef != nil {
		req.WithReplay(policyDef.HasRetries())
	}
	defer req.Close()

	if incomingMD, ok := metadata.FromIncomingContext(ctx); ok {
		req.WithMetadata(incomingMD)
	}

	policyRunner := resiliency.NewRunner[*invokeServiceResp](ctx, policyDef)
	resp, err := policyRunner(func(ctx context.Context) (*invokeServiceResp, error) {
		rResp := &invokeServiceResp{}
		imr, rErr := a.directMessaging.Invoke(ctx, in.GetId(), req)
		if imr != nil {
			// Read the entire message in memory then close imr
			pd, pdErr := imr.ProtoWithData()
			imr.Close()
			if pd != nil {
				rResp.message = pd.GetMessage()
			}

			// If we have an error, set it only if rErr is not already set
			if pdErr != nil && rErr == nil {
				rErr = pdErr
			}
		}
		if rErr != nil {
			return rResp, messages.ErrDirectInvoke.WithFormat(in.GetId(), rErr)
		}

		rResp.headers = invokev1.InternalMetadataToGrpcMetadata(ctx, imr.Headers(), true)

		if imr.IsHTTPResponse() {
			if invokeServiceHTTPDeprecationNoticeShown.CompareAndSwap(false, true) {
				apiServerLogger.Warn("[DEPRECATION NOTICE] Invocation path of gRPC -> HTTP is deprecated and will be removed in the future.")
			}
			var errorMessage string
			if rResp.message != nil && rResp.message.GetData() != nil {
				errorMessage = string(rResp.message.GetData().GetValue())
			}
			code := int(imr.Status().GetCode())
			// If the status is OK, will be nil
			rErr = invokev1.ErrorFromHTTPResponseCode(code, errorMessage)
			// Populate http status code to header
			rResp.headers.Set(daprHTTPStatusHeader, strconv.Itoa(code))
		} else {
			// If the status is OK, will be nil
			rErr = invokev1.ErrorFromInternalStatus(imr.Status())
			// Only include trailers if appchannel uses gRPC
			rResp.trailers = invokev1.InternalMetadataToGrpcMetadata(ctx, imr.Trailers(), false)
		}

		return rResp, rErr
	})

	var message *commonv1pb.InvokeResponse
	if resp != nil {
		if resp.headers != nil {
			grpc.SetHeader(ctx, resp.headers)
		}
		if resp.trailers != nil {
			grpc.SetTrailer(ctx, resp.trailers)
		}
		message = resp.message
	}

	// In this case, there was an error with the actual request or a resiliency policy stopped the request.
	if err != nil {
		// Check if it's returned by status.Errorf
		_, ok := err.(interface{ GRPCStatus() *status.Status })
		if ok || errors.Is(err, context.DeadlineExceeded) || breaker.IsErrorPermanent(err) {
			return nil, err
		}
	}

	return message, err
}

func (a *api) BulkPublishEventAlpha1(ctx context.Context, in *runtimev1pb.BulkPublishRequest) (*runtimev1pb.BulkPublishResponse, error) {
	thepubsub, pubsubName, topic, rawPayload, validationErr := a.validateAndGetPubsubAndTopic(in.GetPubsubName(), in.GetTopic(), in.GetMetadata())

	if validationErr != nil {
		apiServerLogger.Debug(validationErr)
		return &runtimev1pb.BulkPublishResponse{}, validationErr
	}

	span := diagUtils.SpanFromContext(ctx)

	spanMap := map[int]otelTrace.Span{}
	// closeChildSpans method is called on every respond() call in all return paths in the following block of code.
	closeChildSpans := func(_ context.Context, err error) {
		for _, span := range spanMap {
			diag.UpdateSpanStatusFromGRPCError(span, err)
			span.End()
		}
	}

	features := thepubsub.Features()
	entryIdSet := make(map[string]struct{}, len(in.GetEntries())) //nolint:stylecheck

	entries := make([]pubsub.BulkMessageEntry, len(in.GetEntries()))
	for i, entry := range in.GetEntries() {
		// Validate entry_id
		if _, ok := entryIdSet[entry.GetEntryId()]; ok || entry.GetEntryId() == "" {
			err := apierrors.PubSub(pubsubName).WithAppError(
				a.AppID(), errors.New("entryId is duplicated or not present for entry"),
			).WithTopic(topic).MarshalEvents()
			apiServerLogger.Debug(err)
			return &runtimev1pb.BulkPublishResponse{}, err
		}
		entryIdSet[entry.GetEntryId()] = struct{}{}
		entries[i].EntryId = entry.GetEntryId()
		entries[i].ContentType = entry.GetContentType()
		entries[i].Event = entry.GetEvent()
		// Populate entry metadata with request level metadata. Entry level metadata keys
		// override request level metadata.
		if entry.GetMetadata() != nil {
			entries[i].Metadata = utils.PopulateMetadataForBulkPublishEntry(in.GetMetadata(), entry.GetMetadata())
		}

		if !rawPayload {
			// Extract trace context from context.
			_, childSpan := diag.StartGRPCProducerSpanChildFromParent(ctx, span, "/dapr.proto.runtime.v1.Dapr/BulkPublishEventAlpha1/")
			traceID, traceState := diag.TraceIDAndStateFromSpan(childSpan)

			// For multiple events in a single bulk call traceParent is different for each event.
			// Populate W3C traceparent to cloudevent envelope
			spanMap[i] = childSpan

			envelope, err := runtimePubsub.NewCloudEvent(&runtimePubsub.CloudEvent{
				Source:          a.Universal.AppID(),
				Topic:           topic,
				DataContentType: entries[i].ContentType,
				Data:            entries[i].Event,
				TraceID:         traceID,
				TraceState:      traceState,
				Pubsub:          pubsubName,
			}, entries[i].Metadata)
			if err != nil {
				nerr := apierrors.PubSub(pubsubName).WithAppError(
					a.AppID(), err,
				).CloudEventCreation()
				apiServerLogger.Debug(nerr)
				closeChildSpans(ctx, nerr)
				return &runtimev1pb.BulkPublishResponse{}, nerr
			}

			pubsub.ApplyMetadata(envelope, features, entries[i].Metadata)

			entries[i].Event, err = json.Marshal(envelope)
			if err != nil {
				nerr := apierrors.PubSub(pubsubName).WithAppError(
					a.AppID(), err,
				).WithTopic(topic).MarshalEnvelope()
				apiServerLogger.Debug(nerr)
				closeChildSpans(ctx, nerr)
				return &runtimev1pb.BulkPublishResponse{}, nerr
			}
		}
	}

	req := pubsub.BulkPublishRequest{
		PubsubName: pubsubName,
		Topic:      topic,
		Entries:    entries,
		Metadata:   in.GetMetadata(),
	}

	start := time.Now()
	// err is only nil if all entries are successfully published.
	// For partial success, err is not nil and res contains the failed entries.
	res, err := a.pubsubAdapter.BulkPublish(ctx, &req)

	elapsed := diag.ElapsedSince(start)
	eventsPublished := int64(len(req.Entries))

	if len(res.FailedEntries) != 0 {
		eventsPublished -= int64(len(res.FailedEntries))
	}
	diag.DefaultComponentMonitoring.BulkPubsubEgressEvent(context.Background(), pubsubName, topic, err == nil, eventsPublished, elapsed)

	// BulkPublishResponse contains all failed entries from the request.
	// If there are no failed entries, then the failedEntries array will be empty.
	bulkRes := runtimev1pb.BulkPublishResponse{}

	if err != nil {
		var nerr error
		// Only respond with error if it is permission denied or not found.
		// On error, the response will be empty.
		switch {
		case errors.As(err, &runtimePubsub.NotAllowedError{}):
			nerr = apierrors.PubSub(pubsubName).PublishForbidden(topic, a.AppID(), err)
		case errors.As(err, &runtimePubsub.NotFoundError{}):
			nerr = apierrors.PubSub(pubsubName).TestNotFound(topic, err)
		default:
			nerr = apierrors.PubSub(pubsubName).PublishMessage(topic, err)
		}

		apiServerLogger.Debug(nerr)
		closeChildSpans(ctx, nerr)
		return &bulkRes, nerr
	}

	bulkRes.FailedEntries = make([]*runtimev1pb.BulkPublishResponseFailedEntry, 0, len(res.FailedEntries))
	for _, r := range res.FailedEntries {
		resEntry := runtimev1pb.BulkPublishResponseFailedEntry{EntryId: r.EntryId}
		if r.Error != nil {
			resEntry.Error = r.Error.Error()
		}
		bulkRes.FailedEntries = append(bulkRes.GetFailedEntries(), &resEntry)
	}
	closeChildSpans(ctx, nil)
	// even on partial failures, err is nil. As when error is set, the response is expected to not be processed.
	return &bulkRes, nil
}

func (a *api) InvokeBinding(ctx context.Context, in *runtimev1pb.InvokeBindingRequest) (*runtimev1pb.InvokeBindingResponse, error) {
	req := &bindings.InvokeRequest{
		Metadata:  make(map[string]string, len(in.GetMetadata())),
		Operation: bindings.OperationKind(in.GetOperation()),
		Data:      in.GetData(),
	}
	for key, val := range in.GetMetadata() {
		req.Metadata[key] = val
	}

	// this is for the http binding, so dont need grpc-trace-bin
	span := diagUtils.SpanFromContext(ctx)
	sc := span.SpanContext()
	tp := diag.SpanContextToW3CString(sc)
	if span != nil {
		if _, ok := req.Metadata[diagConsts.TraceparentHeader]; !ok {
			req.Metadata[diagConsts.TraceparentHeader] = tp
		}
		if _, ok := req.Metadata[diagConsts.TracestateHeader]; !ok {
			if sc.TraceState().Len() > 0 {
				req.Metadata[diagConsts.TracestateHeader] = diag.TraceStateToW3CString(sc)
			}
		}
	}

	// Allow for distributed tracing by passing context metadata.
	if incomingMD, ok := metadata.FromIncomingContext(ctx); ok {
		if baggageValues := incomingMD[diagConsts.BaggageHeader]; len(baggageValues) > 0 {
			baggageString := strings.Join(baggageValues, ",")
			baggage, err := otelbaggage.Parse(baggageString)
			if err != nil {
				return nil, err
			}
			ctx = otelbaggage.ContextWithBaggage(ctx, baggage)
			req.Metadata[diagConsts.BaggageHeader] = baggageString
		}

		for key, val := range incomingMD {
			sanitizedKey := invokev1.ReservedGRPCMetadataToDaprPrefixHeader(key)
			// Not to overwrite the existing metadata
			// But if the key is traceparent or tracestate, we allow overwrite the existing metadata.
			if _, exist := req.Metadata[sanitizedKey]; !exist || (key == diagConsts.TraceparentHeader || key == diagConsts.TracestateHeader) {
				req.Metadata[sanitizedKey] = val[0]
			}
		}
	}

	r := &runtimev1pb.InvokeBindingResponse{}
	start := time.Now()
	resp, err := a.sendToOutputBindingFn(ctx, in.GetName(), req)
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.OutputBindingEvent(context.Background(), in.GetName(), in.GetOperation(), err == nil, elapsed)

	// Some bindings have metadata in the response even in case of error
	if resp != nil {
		for k, v := range resp.Metadata {
			grpc.SetHeader(ctx, grpcMetadata.Pairs(metadataPrefix+k, v))
		}
	}

	if err != nil {
		richError := apierrors.Basic(codes.Internal, http.StatusInternalServerError, errorcodes.BindingInvokeOutputBinding, fmt.Sprintf(messages.ErrInvokeOutputBinding, in.GetName(), err.Error()))
		apiServerLogger.Debug(richError)
		return r, richError
	}

	if resp != nil {
		r.Data = resp.Data
		r.Metadata = resp.Metadata
	}

	return r, nil
}

func (a *api) GetBulkState(ctx context.Context, in *runtimev1pb.GetBulkStateRequest) (*runtimev1pb.GetBulkStateResponse, error) {
	bulkResp := &runtimev1pb.GetBulkStateResponse{}
	store, err := a.Universal.GetStateStore(in.GetStoreName())
	if err != nil {
		// Error has already been logged
		return bulkResp, err
	}

	if len(in.GetKeys()) == 0 {
		return bulkResp, nil
	}

	var key string
	reqs := make([]state.GetRequest, len(in.GetKeys()))
	for i, k := range in.GetKeys() {
		key, err = stateLoader.GetModifiedStateKey(k, in.GetStoreName(), a.Universal.AppID())
		if err != nil {
			return &runtimev1pb.GetBulkStateResponse{}, err
		}
		r := state.GetRequest{
			Key:      key,
			Metadata: in.GetMetadata(),
		}
		reqs[i] = r
	}

	start := time.Now()
	policyDef := a.Universal.Resiliency().ComponentOutboundPolicy(in.GetStoreName(), resiliency.Statestore)
	bgrPolicyRunner := resiliency.NewRunner[[]state.BulkGetResponse](ctx, policyDef)
	responses, err := bgrPolicyRunner(func(ctx context.Context) ([]state.BulkGetResponse, error) {
		return store.BulkGet(ctx, reqs, state.BulkGetOpts{
			Parallelism: int(in.GetParallelism()),
		})
	})

	elapsed := diag.ElapsedSince(start)
	diag.DefaultComponentMonitoring.StateInvoked(ctx, in.GetStoreName(), diag.BulkGet, err == nil, elapsed)

	if err != nil {
		return bulkResp, err
	}

	bulkResp.Items = make([]*runtimev1pb.BulkStateItem, len(responses))
	for i := range responses {
		item := &runtimev1pb.BulkStateItem{
			Key:      stateLoader.GetOriginalStateKey(responses[i].Key),
			Data:     responses[i].Data,
			Etag:     stringValueOrEmpty(responses[i].ETag),
			Metadata: responses[i].Metadata,
			Error:    responses[i].Error,
		}
		bulkResp.Items[i] = item
	}

	if encryption.EncryptedStateStore(in.GetStoreName()) {
		for i := range bulkResp.GetItems() {
			if bulkResp.GetItems()[i].GetError() != "" || len(bulkResp.GetItems()[i].GetData()) == 0 {
				bulkResp.Items[i].Data = nil
				continue
			}

			val, err := encryption.TryDecryptValue(in.GetStoreName(), bulkResp.GetItems()[i].GetData())
			if err != nil {
				apiServerLogger.Debugf("Bulk get error: %v", err)
				bulkResp.Items[i].Data = nil
				bulkResp.Items[i].Error = err.Error()
				continue
			}

			bulkResp.Items[i].Data = val
		}
	}

	return bulkResp, nil
}

func (a *api) GetState(ctx context.Context, in *runtimev1pb.GetStateRequest) (*runtimev1pb.GetStateResponse, error) {
	store, err := a.Universal.GetStateStore(in.GetStoreName())
	if err != nil {
		// Error has already been logged
		return &runtimev1pb.GetStateResponse{}, err
	}
	key, err := stateLoader.GetModifiedStateKey(in.GetKey(), in.GetStoreName(), a.Universal.AppID())
	if err != nil {
		return &runtimev1pb.GetStateResponse{}, err
	}
	req := &state.GetRequest{
		Key:      key,
		Metadata: in.GetMetadata(),
		Options: state.GetStateOption{
			Consistency: stateConsistencyToString(in.GetConsistency()),
		},
	}

	start := time.Now()
	policyRunner := resiliency.NewRunner[*state.GetResponse](ctx,
		a.Universal.Resiliency().ComponentOutboundPolicy(in.GetStoreName(), resiliency.Statestore),
	)
	getResponse, err := policyRunner(func(ctx context.Context) (*state.GetResponse, error) {
		return store.Get(ctx, req)
	})
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.StateInvoked(ctx, in.GetStoreName(), diag.Get, err == nil, elapsed)

	if err != nil {
		kerr, ok := kiterrors.FromError(err)
		if ok {
			err = kerr.GRPCStatus().Err()
		} else {
			err = apierrors.Basic(codes.Internal, http.StatusInternalServerError, errorcodes.StateGet, fmt.Sprintf(messages.ErrStateGet, in.GetKey(), in.GetStoreName(), err.Error()))
		}

		a.logger.Debug(err)
		return &runtimev1pb.GetStateResponse{}, err
	}

	if getResponse == nil {
		getResponse = &state.GetResponse{}
	}
	if encryption.EncryptedStateStore(in.GetStoreName()) {
		val, err := encryption.TryDecryptValue(in.GetStoreName(), getResponse.Data)
		if err != nil {
			err = apierrors.Basic(codes.Internal, http.StatusInternalServerError, errorcodes.StateGet, fmt.Sprintf(messages.ErrStateGet, in.GetKey(), in.GetStoreName(), err.Error()))
			a.logger.Debug(err)
			return &runtimev1pb.GetStateResponse{}, err
		}

		getResponse.Data = val
	}

	response := &runtimev1pb.GetStateResponse{}
	if getResponse != nil {
		response.Etag = stringValueOrEmpty(getResponse.ETag)
		response.Data = getResponse.Data
		response.Metadata = getResponse.Metadata
	}
	return response, nil
}

func (a *api) SaveState(ctx context.Context, in *runtimev1pb.SaveStateRequest) (*emptypb.Empty, error) {
	empty := &emptypb.Empty{}

	store, err := a.Universal.GetStateStore(in.GetStoreName())
	if err != nil {
		// Error has already been logged
		return empty, err
	}

	l := len(in.GetStates())
	if l == 0 {
		return empty, nil
	}

	reqs := make([]state.SetRequest, l)
	for i, s := range in.GetStates() {
		if len(s.GetKey()) == 0 {
			return empty, apierrors.Basic(codes.InvalidArgument, http.StatusBadRequest, errorcodes.StateSave, "state key cannot be empty")
		}

		var key string
		key, err = stateLoader.GetModifiedStateKey(s.GetKey(), in.GetStoreName(), a.Universal.AppID())
		if err != nil {
			return empty, err
		}
		req := state.SetRequest{
			Key:      key,
			Metadata: s.GetMetadata(),
		}

		if req.Metadata[contribMetadata.ContentType] == contenttype.JSONContentType {
			err = json.Unmarshal(s.GetValue(), &req.Value)
			if err != nil {
				return empty, err
			}
		} else {
			req.Value = s.GetValue()
		}

		if s.GetEtag() != nil {
			req.ETag = &s.Etag.Value
		}
		if s.GetOptions() != nil {
			req.Options = state.SetStateOption{
				Consistency: stateConsistencyToString(s.GetOptions().GetConsistency()),
				Concurrency: stateConcurrencyToString(s.GetOptions().GetConcurrency()),
			}
		}
		if encryption.EncryptedStateStore(in.GetStoreName()) {
			val, encErr := encryption.TryEncryptValue(in.GetStoreName(), s.GetValue())
			if encErr != nil {
				a.logger.Debug(encErr)
				return empty, encErr
			}

			req.Value = val
		}

		reqs[i] = req
	}

	start := time.Now()
	err = stateLoader.PerformBulkStoreOperation(ctx, reqs,
		a.Universal.Resiliency().ComponentOutboundPolicy(in.GetStoreName(), resiliency.Statestore),
		state.BulkStoreOpts{},
		store.Set,
		store.BulkSet,
	)
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.StateInvoked(ctx, in.GetStoreName(), diag.Set, err == nil, elapsed)

	if err != nil {
		if kerr, ok := kiterrors.FromError(err); ok {
			err = kerr
		} else {
			err = apierrors.Basic(a.getStateErrorCode(err), http.StatusInternalServerError, errorcodes.StateSave, fmt.Sprintf(messages.ErrStateSave, in.GetStoreName(), err.Error()))
		}
		a.logger.Debug(err)
		return empty, err
	}
	return empty, nil
}

// getStateErrorCode takes a state store error and returns the associated etag's grpcCode, if applicable
func (a *api) getStateErrorCode(err error) codes.Code {
	var etagErr *state.ETagError
	if errors.As(err, &etagErr) {
		switch etagErr.Kind() {
		case state.ETagMismatch:
			return codes.Aborted
		case state.ETagInvalid:
			return codes.InvalidArgument
		}
	}

	return codes.Internal
}

func (a *api) DeleteState(ctx context.Context, in *runtimev1pb.DeleteStateRequest) (*emptypb.Empty, error) {
	empty := &emptypb.Empty{}

	store, err := a.Universal.GetStateStore(in.GetStoreName())
	if err != nil {
		// Error has already been logged
		return empty, err
	}

	key, err := stateLoader.GetModifiedStateKey(in.GetKey(), in.GetStoreName(), a.Universal.AppID())
	if err != nil {
		return empty, err
	}
	req := state.DeleteRequest{
		Key:      key,
		Metadata: in.GetMetadata(),
	}
	if in.GetEtag() != nil {
		req.ETag = &in.Etag.Value
	}
	if in.GetOptions() != nil {
		req.Options = state.DeleteStateOption{
			Concurrency: stateConcurrencyToString(in.GetOptions().GetConcurrency()),
			Consistency: stateConsistencyToString(in.GetOptions().GetConsistency()),
		}
	}

	start := time.Now()
	policyRunner := resiliency.NewRunner[any](ctx,
		a.Universal.Resiliency().ComponentOutboundPolicy(in.GetStoreName(), resiliency.Statestore),
	)
	_, err = policyRunner(func(ctx context.Context) (any, error) {
		return nil, store.Delete(ctx, &req)
	})
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.StateInvoked(ctx, in.GetStoreName(), diag.Delete, err == nil, elapsed)

	if err != nil {
		if kerr, ok := kiterrors.FromError(err); ok {
			err = kerr
		} else {
			err = apierrors.Basic(a.getStateErrorCode(err), http.StatusInternalServerError, errorcodes.StateDelete, fmt.Sprintf(messages.ErrStateDelete, in.GetKey(), err.Error()))
		}
		a.logger.Debug(err)
		return empty, err
	}
	return empty, nil
}

func (a *api) DeleteBulkState(ctx context.Context, in *runtimev1pb.DeleteBulkStateRequest) (*emptypb.Empty, error) {
	empty := &emptypb.Empty{}

	store, err := a.Universal.GetStateStore(in.GetStoreName())
	if err != nil {
		// Error has already been logged
		return empty, err
	}

	reqs := make([]state.DeleteRequest, len(in.GetStates()))
	for i, item := range in.GetStates() {
		key, err1 := stateLoader.GetModifiedStateKey(item.GetKey(), in.GetStoreName(), a.Universal.AppID())
		if err1 != nil {
			return empty, err1
		}
		req := state.DeleteRequest{
			Key:      key,
			Metadata: item.GetMetadata(),
		}
		if item.GetEtag() != nil {
			req.ETag = &item.Etag.Value
		}
		if item.GetOptions() != nil {
			req.Options = state.DeleteStateOption{
				Concurrency: stateConcurrencyToString(item.GetOptions().GetConcurrency()),
				Consistency: stateConsistencyToString(item.GetOptions().GetConsistency()),
			}
		}
		reqs[i] = req
	}

	start := time.Now()
	err = stateLoader.PerformBulkStoreOperation(ctx, reqs,
		a.Universal.Resiliency().ComponentOutboundPolicy(in.GetStoreName(), resiliency.Statestore),
		state.BulkStoreOpts{},
		store.Delete,
		store.BulkDelete,
	)
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.StateInvoked(ctx, in.GetStoreName(), diag.BulkDelete, err == nil, elapsed)

	if err != nil {
		if kerr, ok := kiterrors.FromError(err); ok {
			err = kerr
		} else {
			err = apierrors.Basic(a.getStateErrorCode(err), http.StatusInternalServerError, errorcodes.StateBulkDelete, fmt.Sprintf(messages.ErrStateDeleteBulk, in.GetStoreName(), err.Error()))
		}
		a.logger.Debug(err)
		return empty, err
	}

	return empty, nil
}

func extractEtag(req *commonv1pb.StateItem) (bool, string) {
	if req.GetEtag() != nil {
		return true, req.GetEtag().GetValue()
	}
	return false, ""
}

func (a *api) ExecuteStateTransaction(ctx context.Context, in *runtimev1pb.ExecuteStateTransactionRequest) (*emptypb.Empty, error) {
	store, storeErr := a.Universal.GetStateStore(in.GetStoreName())
	if storeErr != nil {
		// Error has already been logged
		return &emptypb.Empty{}, storeErr
	}

	transactionalStore, ok := store.(state.TransactionalStore)
	if !ok || !state.FeatureTransactional.IsPresent(store.Features()) {
		err := apierrors.StateStore(in.GetStoreName()).TransactionsNotSupported()
		apiServerLogger.Debug(err)
		return &emptypb.Empty{}, err
	}

	operations := make([]state.TransactionalStateOperation, 0, len(in.GetOperations()))
	for _, inputReq := range in.GetOperations() {
		req := inputReq.GetRequest()

		hasEtag, etag := extractEtag(req)
		key, err := stateLoader.GetModifiedStateKey(req.GetKey(), in.GetStoreName(), a.Universal.AppID())
		if err != nil {
			return &emptypb.Empty{}, err
		}
		switch state.OperationType(inputReq.GetOperationType()) {
		case state.OperationUpsert:
			setReq := state.SetRequest{
				Key: key,
				// Limitation:
				// components that cannot handle byte array need to deserialize/serialize in
				// component specific way in components-contrib repo.
				Value:    req.GetValue(),
				Metadata: req.GetMetadata(),
			}

			if hasEtag {
				setReq.ETag = &etag
			}
			if req.GetOptions() != nil {
				setReq.Options = state.SetStateOption{
					Concurrency: stateConcurrencyToString(req.GetOptions().GetConcurrency()),
					Consistency: stateConsistencyToString(req.GetOptions().GetConsistency()),
				}
			}

			operations = append(operations, setReq)

		case state.OperationDelete:
			delReq := state.DeleteRequest{
				Key:      key,
				Metadata: req.GetMetadata(),
			}

			if hasEtag {
				delReq.ETag = &etag
			}
			if req.GetOptions() != nil {
				delReq.Options = state.DeleteStateOption{
					Concurrency: stateConcurrencyToString(req.GetOptions().GetConcurrency()),
					Consistency: stateConsistencyToString(req.GetOptions().GetConsistency()),
				}
			}

			operations = append(operations, delReq)

		default:
			err = apierrors.Basic(codes.Unimplemented, http.StatusInternalServerError, errorcodes.StateNotSupportedOperation, fmt.Sprintf(messages.ErrNotSupportedStateOperation, inputReq.GetOperationType()))
			apiServerLogger.Debug(err)
			return &emptypb.Empty{}, err
		}
	}

	if maxMulti, ok := store.(state.TransactionalStoreMultiMaxSize); ok {
		max := maxMulti.MultiMaxSize()
		if max > 0 && len(operations) > max {
			err := apierrors.StateStore(in.GetStoreName()).TooManyTransactionalOps(len(operations), max)
			apiServerLogger.Debug(err)
			return &emptypb.Empty{}, err
		}
	}

	if encryption.EncryptedStateStore(in.GetStoreName()) {
		for i, op := range operations {
			switch req := op.(type) {
			case state.SetRequest:
				data := []byte(fmt.Sprintf("%v", req.Value))
				val, err := encryption.TryEncryptValue(in.GetStoreName(), data)
				if err != nil {
					err = apierrors.Basic(codes.Internal, http.StatusInternalServerError, errorcodes.StateTransaction, fmt.Sprintf(messages.ErrStateTransaction, err.Error()))
					apiServerLogger.Debug(err)
					return &emptypb.Empty{}, err
				}

				req.Value = val
				operations[i] = req
			}
		}
	}

	outboxEnabled := a.outbox.Enabled(in.GetStoreName())
	if outboxEnabled {
		span := diagUtils.SpanFromContext(ctx)
		traceID, traceState := diag.TraceIDAndStateFromSpan(span)
		ops, err := a.outbox.PublishInternal(ctx, in.GetStoreName(), operations, a.Universal.AppID(), traceID, traceState)
		if err != nil {
			nerr := apierrors.PubSubOutbox(a.AppID(), err)
			apiServerLogger.Debug(nerr)
			return &emptypb.Empty{}, nerr
		}

		operations = ops
	}

	start := time.Now()
	policyRunner := resiliency.NewRunner[struct{}](ctx,
		a.Universal.Resiliency().ComponentOutboundPolicy(in.GetStoreName(), resiliency.Statestore),
	)
	storeReq := &state.TransactionalStateRequest{
		Operations: operations,
		Metadata:   in.GetMetadata(),
	}
	_, err := policyRunner(func(ctx context.Context) (struct{}, error) {
		return struct{}{}, transactionalStore.Multi(ctx, storeReq)
	})
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.StateInvoked(ctx, in.GetStoreName(), diag.StateTransaction, err == nil, elapsed)

	if err != nil {
		err = apierrors.Basic(codes.Internal, http.StatusInternalServerError, errorcodes.StateTransaction, fmt.Sprintf(messages.ErrStateTransaction, err.Error()))
		apiServerLogger.Debug(err)
		return &emptypb.Empty{}, err
	}
	return &emptypb.Empty{}, nil
}

func (a *api) GetActorState(ctx context.Context, in *runtimev1pb.GetActorStateRequest) (*runtimev1pb.GetActorStateResponse, error) {
	astate, err := a.ActorState(ctx)
	if err != nil {
		apiServerLogger.Debug(err)
		return nil, err
	}

	actorType := in.GetActorType()
	actorID := in.GetActorId()
	key := in.GetKey()

	req := actorapi.GetStateRequest{
		ActorType: actorType,
		ActorID:   actorID,
		Key:       key,
	}

	resp, err := astate.Get(ctx, &req, true)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			apiServerLogger.Debug(err)
			return nil, err
		}

		err = messages.ErrActorStateGet.WithFormat(err)
		apiServerLogger.Debug(err)
		return nil, err
	}

	return &runtimev1pb.GetActorStateResponse{
		Data:     resp.Data,
		Metadata: resp.Metadata,
	}, nil
}

func (a *api) ExecuteActorStateTransaction(ctx context.Context, in *runtimev1pb.ExecuteActorStateTransactionRequest) (*emptypb.Empty, error) {
	astate, err := a.ActorState(ctx)
	if err != nil {
		apiServerLogger.Debug(err)
		return nil, err
	}

	actorType := in.GetActorType()
	actorID := in.GetActorId()
	actorOps := []actorapi.TransactionalOperation{}

	for _, op := range in.GetOperations() {
		var actorOp actorapi.TransactionalOperation
		switch op.GetOperationType() {
		case string(state.OperationUpsert):
			setReq := map[string]any{
				"key":   op.GetKey(),
				"value": op.GetValue().GetValue(),
				// Actor state do not user other attributes from state request.
			}
			if meta := op.GetMetadata(); len(meta) > 0 {
				setReq["metadata"] = meta
			}

			actorOp = actorapi.TransactionalOperation{
				Operation: actorapi.Upsert,
				Request:   setReq,
			}
		case string(state.OperationDelete):
			delReq := map[string]interface{}{
				"key": op.GetKey(),
				// Actor state do not user other attributes from state request.
			}

			actorOp = actorapi.TransactionalOperation{
				Operation: actorapi.Delete,
				Request:   delReq,
			}

		default:
			err = apierrors.Basic(codes.Unimplemented, http.StatusInternalServerError, errorcodes.StateNotSupportedOperation, fmt.Sprintf(messages.ErrNotSupportedStateOperation, op.GetOperationType()))
			apiServerLogger.Debug(err)
			return nil, err
		}

		actorOps = append(actorOps, actorOp)
	}

	req := actorapi.TransactionalRequest{
		ActorID:    actorID,
		ActorType:  actorType,
		Operations: actorOps,
	}

	err = astate.TransactionalStateOperation(ctx, false, &req, true)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			apiServerLogger.Debug(err)
			return nil, err
		}

		err = messages.ErrActorStateTransactionSave.WithFormat(err)
		apiServerLogger.Debug(err)
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (a *api) InvokeActor(ctx context.Context, in *runtimev1pb.InvokeActorRequest) (*runtimev1pb.InvokeActorResponse, error) {
	response := &runtimev1pb.InvokeActorResponse{}

	router, err := a.ActorRouter(ctx)
	if err != nil {
		return nil, err
	}

	if in.Metadata == nil {
		in.Metadata = make(map[string]string)
	}
	in.Metadata["Dapr-API-Call"] = "true"

	req := in.ToInternalInvokeRequest()

	// Unlike other actor calls, resiliency is handled here for invocation.
	// This is due to actor invocation involving a lookup for the host.
	policyDef := a.Universal.Resiliency().ActorPreLockPolicy(in.GetActorType(), in.GetActorId())
	policyRunner := resiliency.NewRunner[*internalv1pb.InternalInvokeResponse](ctx, policyDef)
	res, err := policyRunner(func(ctx context.Context) (*internalv1pb.InternalInvokeResponse, error) {
		return router.Call(ctx, req)
	})
	if err != nil {
		if _, ok := status.FromError(err); ok {
			apiServerLogger.Debug(err)
			return nil, err
		}
		if !actorerrors.Is(err) {
			err = messages.ErrActorInvoke.WithFormat(err)
			apiServerLogger.Debug(err)
			return response, err
		}
	}

	if res != nil {
		response.Data = res.GetMessage().GetData().GetValue()
	}
	return response, nil
}

func stringValueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

func (a *api) getConfigurationStore(name string) (configuration.Store, error) {
	if a.CompStore().ConfigurationsLen() == 0 {
		err := apierrors.Basic(codes.FailedPrecondition, http.StatusInternalServerError, errorcodes.ConfigurationStoreNotConfigured, messages.ErrConfigurationStoresNotConfigured)
		return nil, err
	}

	conf, ok := a.CompStore().GetConfiguration(name)
	if !ok {
		err := apierrors.Basic(codes.InvalidArgument, http.StatusInternalServerError, errorcodes.ConfigurationStoreNotFound, fmt.Sprintf(messages.ErrConfigurationStoreNotFound, name))
		return nil, err
	}
	return conf, nil
}

func (a *api) GetConfiguration(ctx context.Context, in *runtimev1pb.GetConfigurationRequest) (*runtimev1pb.GetConfigurationResponse, error) {
	response := &runtimev1pb.GetConfigurationResponse{}

	store, err := a.getConfigurationStore(in.GetStoreName())
	if err != nil {
		apiServerLogger.Debug(err)
		return response, err
	}

	req := configuration.GetRequest{
		Keys:     in.GetKeys(),
		Metadata: in.GetMetadata(),
	}

	start := time.Now()
	policyRunner := resiliency.NewRunner[*configuration.GetResponse](ctx,
		a.Universal.Resiliency().ComponentOutboundPolicy(in.GetStoreName(), resiliency.Configuration),
	)
	getResponse, err := policyRunner(func(ctx context.Context) (*configuration.GetResponse, error) {
		return store.Get(ctx, &req)
	})
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.ConfigurationInvoked(ctx, in.GetStoreName(), diag.Get, err == nil, elapsed)

	if err != nil {
		richError := apierrors.Basic(codes.Internal, http.StatusInternalServerError, errorcodes.ConfigurationGet, fmt.Sprintf(messages.ErrConfigurationGet, req.Keys, in.GetStoreName(), err.Error()))
		apiServerLogger.Debug(richError)
		return response, richError
	}

	if getResponse != nil {
		cachedItems := make(map[string]*commonv1pb.ConfigurationItem, len(getResponse.Items))
		for k, v := range getResponse.Items {
			cachedItems[k] = &commonv1pb.ConfigurationItem{
				Metadata: v.Metadata,
				Value:    v.Value,
				Version:  v.Version,
			}
		}
		response.Items = cachedItems
	}

	return response, nil
}

// TODO: Remove this method when the alpha API is removed.
func (a *api) GetConfigurationAlpha1(ctx context.Context, in *runtimev1pb.GetConfigurationRequest) (*runtimev1pb.GetConfigurationResponse, error) {
	return a.GetConfiguration(ctx, in)
}

type configurationEventHandler struct {
	readyCh      chan struct{}
	readyClosed  bool
	lock         sync.Mutex
	api          *api
	storeName    string
	serverStream runtimev1pb.Dapr_SubscribeConfigurationAlpha1Server //nolint:nosnakecase
}

func (h *configurationEventHandler) ready() {
	if !h.readyClosed {
		close(h.readyCh)
		h.readyClosed = true
	}
}

func (h *configurationEventHandler) updateEventHandler(ctx context.Context, e *configuration.UpdateEvent) error {
	// Blocks until the first message is sent
	<-h.readyCh

	// Calling Send on a gRPC stream from multiple goroutines at the same time is not supported
	h.lock.Lock()
	defer h.lock.Unlock()

	items := make(map[string]*commonv1pb.ConfigurationItem, len(e.Items))
	for k, v := range e.Items {
		items[k] = &commonv1pb.ConfigurationItem{
			Value:    v.Value,
			Version:  v.Version,
			Metadata: v.Metadata,
		}
	}

	err := h.serverStream.Send(&runtimev1pb.SubscribeConfigurationResponse{
		Items: items,
		Id:    e.ID,
	})
	if err != nil {
		apiServerLogger.Debug(err)
		return err
	}
	return nil
}

func (a *api) SubscribeConfiguration(request *runtimev1pb.SubscribeConfigurationRequest, stream runtimev1pb.Dapr_SubscribeConfigurationServer) error { //nolint:nosnakecase
	store, err := a.getConfigurationStore(request.GetStoreName())
	if err != nil {
		apiServerLogger.Debug(err)
		return err
	}

	handler := &configurationEventHandler{
		readyCh:      make(chan struct{}),
		api:          a,
		storeName:    request.GetStoreName(),
		serverStream: stream,
	}
	// Prevents a leak if we return with an error
	defer handler.ready()

	// Subscribe
	subscribeCtx, subscribeCancel := context.WithCancel(stream.Context())
	defer subscribeCancel()
	slices.Sort(request.GetKeys())
	subscribeID, err := a.subscribeConfiguration(subscribeCtx, request, handler, store)
	if err != nil {
		// Error has already been logged
		return err
	}

	// Send subscription ID
	// This is primarily meant for backwards-compatibility with using the Unsubscribe method
	err = handler.serverStream.Send(&runtimev1pb.SubscribeConfigurationResponse{
		Id: subscribeID,
	})
	if err != nil {
		apiServerLogger.Debug(err)
		return err
	}

	stop := make(chan struct{})
	a.CompStore().AddConfigurationSubscribe(subscribeID, stop)

	// We have sent the first message, so signal that we're ready to send messages in the stream
	handler.ready()

	// Wait until the channel is stopped or until the client disconnects
	select {
	case <-stream.Context().Done():
	case <-stop:
	}

	// Cancel the context here to immediately stop sending messages while we unsubscribe
	subscribeCancel()

	// Unsubscribe
	// We must use a background context here because stream.Context is likely canceled already
	err = a.unsubscribeConfiguration(context.Background(), subscribeID, request.GetStoreName(), store)
	if err != nil {
		// Error has already been logged
		return err
	}

	// Delete the subscription ID (and the stop channel) if we got here because of the context being canceled
	a.CompStore().DeleteConfigurationSubscribe(subscribeID)

	return nil
}

func (a *api) subscribeConfiguration(ctx context.Context, request *runtimev1pb.SubscribeConfigurationRequest, handler *configurationEventHandler, store configuration.Store) (subscribeID string, err error) {
	componentReq := &configuration.SubscribeRequest{
		Keys:     request.GetKeys(),
		Metadata: request.GetMetadata(),
	}

	// TODO(@laurence) deal with failed subscription and retires
	start := time.Now()
	policyRunner := resiliency.NewRunner[string](ctx,
		a.Universal.Resiliency().ComponentOutboundPolicy(request.GetStoreName(), resiliency.Configuration),
	)
	subscribeID, err = policyRunner(func(ctx context.Context) (string, error) {
		return store.Subscribe(ctx, componentReq, handler.updateEventHandler)
	})
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.ConfigurationInvoked(context.Background(), request.GetStoreName(), diag.ConfigurationSubscribe, err == nil, elapsed)

	if err != nil {
		richError := apierrors.Basic(codes.InvalidArgument, http.StatusInternalServerError, errorcodes.ConfigurationSubscribe, fmt.Sprintf(messages.ErrConfigurationSubscribe, componentReq.Keys, request.GetStoreName(), err))
		apiServerLogger.Debug(richError)
		return "", richError
	}

	return subscribeID, nil
}

func (a *api) unsubscribeConfiguration(ctx context.Context, subscribeID string, storeName string, store configuration.Store) error {
	policyRunner := resiliency.NewRunner[struct{}](ctx,
		a.Universal.Resiliency().ComponentOutboundPolicy(storeName, resiliency.Configuration),
	)
	start := time.Now()
	storeReq := &configuration.UnsubscribeRequest{
		ID: subscribeID,
	}
	_, err := policyRunner(func(ctx context.Context) (struct{}, error) {
		return struct{}{}, store.Unsubscribe(ctx, storeReq)
	})
	elapsed := diag.ElapsedSince(start)

	diag.DefaultComponentMonitoring.ConfigurationInvoked(context.Background(), storeName, diag.ConfigurationUnsubscribe, err == nil, elapsed)

	return err
}

// TODO: Remove this method when the alpha API is removed.
func (a *api) SubscribeConfigurationAlpha1(request *runtimev1pb.SubscribeConfigurationRequest, configurationServer runtimev1pb.Dapr_SubscribeConfigurationAlpha1Server) error { //nolint:nosnakecase
	return a.SubscribeConfiguration(request, configurationServer.(runtimev1pb.Dapr_SubscribeConfigurationServer))
}

// This method is deprecated and exists for backwards-compatibility only.
// It causes an active SubscribeConfiguration RPC for the given subscription ID to be stopped if active
func (a *api) UnsubscribeConfiguration(ctx context.Context, request *runtimev1pb.UnsubscribeConfigurationRequest) (*runtimev1pb.UnsubscribeConfigurationResponse, error) {
	subscribeID := request.GetId()
	_, ok := a.CompStore().GetConfigurationSubscribe(subscribeID)
	if !ok {
		// TODO: Make this response provide error codes (so it gets recorded at the end/middleware) so we don't have to record it early
		diag.RecordErrorCode(&errorcodes.ConfigurationUnsubscribe)
		return &runtimev1pb.UnsubscribeConfigurationResponse{
			Ok:      false,
			Message: fmt.Sprintf(messages.ErrConfigurationUnsubscribe, subscribeID, "subscription does not exist"),
		}, nil
	}

	a.logger.Warn("Unsubscribing using UnsubscribeConfiguration is deprecated. Disconnect from the SubscribeConfiguration RPC instead.")

	// This causes the subscription with the given ID to be stopped and that stream to be aborted, if active
	a.CompStore().DeleteConfigurationSubscribe(subscribeID)

	return &runtimev1pb.UnsubscribeConfigurationResponse{
		Ok: true,
	}, nil
}

// TODO: Remove this method when the alpha API is removed.
func (a *api) UnsubscribeConfigurationAlpha1(ctx context.Context, request *runtimev1pb.UnsubscribeConfigurationRequest) (*runtimev1pb.UnsubscribeConfigurationResponse, error) {
	return a.UnsubscribeConfiguration(ctx, request)
}

func (a *api) Close() error {
	defer a.wg.Wait()

	if a.closed.CompareAndSwap(false, true) {
		close(a.closeCh)
	}

	a.CompStore().DeleteAllConfigurationSubscribe()

	return nil
}
