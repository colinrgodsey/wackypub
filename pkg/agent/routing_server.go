package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// RoutingServer is a reflection-based dynamic gRPC routing proxy.
// It intercepts all incoming RPCs via a grpc.UnknownServiceHandler, routing
// bridged agents (defined in REMOTE_MANIFEST) to their configured bridge process
// via gRPC client conn forwarding (conn.Invoke / conn.NewStream), and native agents
// directly to the local in-process AgentSDK via Go reflection.
type RoutingServer struct {
	sdk *AgentSDK
}

// NewRoutingServer creates a dynamic routing proxy for sdk.
func NewRoutingServer(sdk *AgentSDK) *RoutingServer {
	return &RoutingServer{sdk: sdk}
}

// UnknownServiceHandler returns a grpc.ServerOption registering StreamHandler
// as the unknown service handler for the gRPC server.
func (s *RoutingServer) UnknownServiceHandler() grpc.ServerOption {
	return grpc.UnknownServiceHandler(s.StreamHandler)
}

func (s *RoutingServer) sdkFor(wsDir string) *AgentSDK {
	if wsDir == "" || wsDir == s.sdk.WorkspaceDir {
		return s.sdk
	}
	target := NewSDK(wsDir)
	target.MaxToolTurns = s.sdk.MaxToolTurns
	target.CommandTimeoutSeconds = s.sdk.CommandTimeoutSeconds
	return target
}

// genericServerStream adapts grpc.ServerStream to grpc.ServerStreamingServer[T].
type genericServerStream[T any] struct {
	grpc.ServerStream
}

func (s *genericServerStream[T]) Send(m *T) error {
	return s.ServerStream.SendMsg(m)
}

// StreamHandler handles all incoming RPCs dynamically without per-method maintenance.
func (s *RoutingServer) StreamHandler(srv any, stream grpc.ServerStream) (err error) {
	fullMethod, ok := grpc.Method(stream.Context())
	if !ok {
		return status.Errorf(codes.Internal, "no method in context")
	}

	defer func() {
		if r := recover(); r != nil {
			if IsUnrecoverable(r) {
				fmt.Fprintf(os.Stderr, "wackypub stdio-serve: fatal unrecoverable panic in RPC %s: %v\n", fullMethod, r)
				panic(r)
			}
			stack := debug.Stack()
			fmt.Fprintf(os.Stderr, "wackypub stdio-serve: recovered panic in RPC %s: %v\n%s\n", fullMethod, r, stack)
			err = status.Errorf(codes.Internal, "internal server error: %v", r)
		} else if err != nil {
			err = ToGRPCError(err)
		}
	}()

	parts := strings.Split(strings.TrimPrefix(fullMethod, "/"), "/")
	if len(parts) != 2 {
		return status.Errorf(codes.Unimplemented, "unrecognized method %q", fullMethod)
	}
	serviceName := parts[0]
	methodName := parts[1]

	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(serviceName + "." + methodName))
	if err != nil {
		return status.Errorf(codes.Unimplemented, "unknown method %q: %v", fullMethod, err)
	}
	methodDesc, ok := desc.(protoreflect.MethodDescriptor)
	if !ok {
		return status.Errorf(codes.Unimplemented, "descriptor %q is not a method", fullMethod)
	}

	inputType := methodDesc.Input()
	inputMsgType, err := protoregistry.GlobalTypes.FindMessageByName(inputType.FullName())
	if err != nil {
		return status.Errorf(codes.Internal, "unknown input message type %q: %v", inputType.FullName(), err)
	}
	req := inputMsgType.New().Interface()
	if err := stream.RecvMsg(req); err != nil {
		return err
	}

	var agentID, wsDir string
	val := reflect.ValueOf(req)
	if m := val.MethodByName("GetAgentId"); m.IsValid() {
		res := m.Call(nil)
		if len(res) > 0 {
			agentID = res[0].String()
		}
	}
	if m := val.MethodByName("GetWorkspaceDir"); m.IsValid() {
		res := m.Call(nil)
		if len(res) > 0 {
			wsDir = res[0].String()
		}
	}

	targetSDK := s.sdkFor(wsDir)

	if testHookRoutingPreDispatch != nil {
		testHookRoutingPreDispatch(methodName, req)
	}
	if os.Getenv("WACKYPUB_FAULT_INJECT") == "unary_panic" {
		if agentID == "__PANIC_AGENT__" || methodName == "FaultInjectPanic" {
			panic("fault injected: simulated unary panic")
		}
	}

	// Special case: InspectAgentLocks stays local (workspace-level query)
	if methodName == "InspectAgentLocks" {
		locksReq, ok := req.(*agentv1.InspectAgentLocksRequest)
		if !ok {
			return status.Errorf(codes.InvalidArgument, "expected InspectAgentLocksRequest")
		}
		resp, err := targetSDK.InspectAgentLocks(stream.Context(), locksReq)
		if err != nil {
			return err
		}
		return stream.SendMsg(resp)
	}

	// Special case: ListAgents merges native and bridged agents
	if methodName == "ListAgents" {
		listReq, ok := req.(*agentv1.ListAgentsRequest)
		if !ok {
			return status.Errorf(codes.InvalidArgument, "expected ListAgentsRequest")
		}
		resp, err := targetSDK.ListAgents(stream.Context(), listReq)
		if err != nil {
			return err
		}
		manifest, err := LoadRemoteManifest(targetSDK.WorkspaceDir)
		if err == nil && manifest != nil && len(manifest.Routes) > 0 {
			seen := make(map[string]bool)
			for _, id := range resp.GetAgentIds() {
				seen[id] = true
			}
			ids := resp.GetAgentIds()
			for id := range manifest.Routes {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			resp.AgentIds = ids
		}
		return stream.SendMsg(resp)
	}

	// All other methods: check REMOTE_MANIFEST for bridged routing
	manifest, err := LoadRemoteManifest(targetSDK.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("loading REMOTE_MANIFEST: %w", err)
	}

	route, bridged := manifest.Lookup(agentID)
	if bridged {
		// Bridged path: forward transparently over the bridge subprocess gRPC connection
		conn, cleanup, err := dialBridgeConn(stream.Context(), targetSDK.WorkspaceDir, agentID, route)
		if err != nil {
			return err
		}
		defer cleanup()

		outputType := methodDesc.Output()
		outputMsgType, err := protoregistry.GlobalTypes.FindMessageByName(outputType.FullName())
		if err != nil {
			return status.Errorf(codes.Internal, "unknown output message type %q: %v", outputType.FullName(), err)
		}

		if methodDesc.IsStreamingServer() {
			cs, err := conn.NewStream(stream.Context(), &grpc.StreamDesc{
				ServerStreams: true,
			}, fullMethod)
			if err != nil {
				return err
			}
			if err := cs.SendMsg(req); err != nil {
				return err
			}
			if err := cs.CloseSend(); err != nil {
				return err
			}
			for {
				chunk := outputMsgType.New().Interface()
				if err := cs.RecvMsg(chunk); err != nil {
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}
				if err := stream.SendMsg(chunk); err != nil {
					return err
				}
			}
		}

		// Unary bridged RPC
		resp := outputMsgType.New().Interface()
		if err := conn.Invoke(stream.Context(), fullMethod, req, resp); err != nil {
			return err
		}
		return stream.SendMsg(resp)
	}

	// Native hot path: dispatch directly against local in-process AgentSDK via reflection
	if methodDesc.IsStreamingServer() {
		switch methodName {
		case "GenerateTurnStream":
			reqTyped, ok := req.(*agentv1.GenerateTurnStreamRequest)
			if !ok {
				return status.Errorf(codes.InvalidArgument, "expected GenerateTurnStreamRequest, got %T", req)
			}
			st := &genericServerStream[agentv1.GenerateTurnStreamResponse]{ServerStream: stream}
			return targetSDK.GenerateTurnStream(reqTyped, st)
		case "AddAndGenerateTurnStream":
			reqTyped, ok := req.(*agentv1.AddAndGenerateTurnStreamRequest)
			if !ok {
				return status.Errorf(codes.InvalidArgument, "expected AddAndGenerateTurnStreamRequest, got %T", req)
			}
			st := &genericServerStream[agentv1.AddAndGenerateTurnStreamResponse]{ServerStream: stream}
			return targetSDK.AddAndGenerateTurnStream(reqTyped, st)
		case "SubscribeSession":
			reqTyped, ok := req.(*agentv1.SubscribeSessionRequest)
			if !ok {
				return status.Errorf(codes.InvalidArgument, "expected SubscribeSessionRequest, got %T", req)
			}
			st := &genericServerStream[agentv1.SubscribeSessionResponse]{ServerStream: stream}
			return targetSDK.SubscribeSession(reqTyped, st)
		default:
			return status.Errorf(codes.Unimplemented, "streaming method %q not implemented on AgentSDK", methodName)
		}
	}

	sdkVal := reflect.ValueOf(targetSDK)
	m := sdkVal.MethodByName(methodName)
	if !m.IsValid() {
		return status.Errorf(codes.Unimplemented, "method %q not implemented on AgentSDK", methodName)
	}

	mType := m.Type()
	if mType.NumIn() != 2 || mType.NumOut() != 2 {
		return status.Errorf(codes.Internal, "method %q has unexpected signature", methodName)
	}

	inArgs := []reflect.Value{
		reflect.ValueOf(stream.Context()),
		reflect.ValueOf(req),
	}
	if !inArgs[1].Type().AssignableTo(mType.In(1)) {
		return status.Errorf(codes.InvalidArgument, "request type %s not assignable to %s for method %s", inArgs[1].Type(), mType.In(1), methodName)
	}

	results := m.Call(inArgs)
	if !results[1].IsNil() {
		if errVal, ok := results[1].Interface().(error); ok {
			return errVal
		}
		return status.Errorf(codes.Internal, "method %q returned non-error in second position", methodName)
	}
	return stream.SendMsg(results[0].Interface())
}
