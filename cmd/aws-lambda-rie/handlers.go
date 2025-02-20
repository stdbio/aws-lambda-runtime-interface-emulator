// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.amzn.com/lambda/core/statejson"
	"go.amzn.com/lambda/interop"
	"go.amzn.com/lambda/rapidcore"
	"go.amzn.com/lambda/rapidcore/env"

	"github.com/go-chi/chi"
	"github.com/google/uuid"

	"io"

	log "github.com/sirupsen/logrus"
)

type Sandbox interface {
	Init(i *interop.Init, invokeTimeoutMs int64)
	Invoke(responseWriter http.ResponseWriter, invoke *interop.Invoke) error
}

type InteropServer interface {
	Init(i *interop.Init, invokeTimeoutMs int64) error
	AwaitInitialized() error
	FastInvoke(w http.ResponseWriter, i *interop.Invoke, direct bool) error
	Reserve(id string, traceID, lambdaSegmentID string) (*rapidcore.ReserveResponse, error)
	Reset(reason string, timeoutMs int64) (*statejson.ResetDescription, error)
	AwaitRelease() (*statejson.InternalStateDescription, error)
	Shutdown(shutdown *interop.Shutdown) *statejson.InternalStateDescription
	InternalState() (*statejson.InternalStateDescription, error)
	CurrentToken() *interop.Token
	Restore(restore *interop.Restore) error
}

var initDone bool

func GetenvWithDefault(key string, defaultValue string) string {
	envValue := os.Getenv(key)

	if envValue == "" {
		return defaultValue
	}

	return envValue
}

func printEndReports(invokeId string, initDuration string, memorySize string, invokeStart time.Time, timeoutDuration time.Duration) {
	// Calcuation invoke duration
	invokeDuration := math.Min(float64(time.Now().Sub(invokeStart).Nanoseconds()),
		float64(timeoutDuration.Nanoseconds())) / float64(time.Millisecond)

	fmt.Println("END RequestId: " + invokeId)
	// We set the Max Memory Used and Memory Size to be the same (whatever it is set to) since there is
	// not a clean way to get this information from rapidcore
	fmt.Printf(
		"REPORT RequestId: %s\t"+
			initDuration+
			"Duration: %.2f ms\t"+
			"Billed Duration: %.f ms\t"+
			"Memory Size: %s MB\t"+
			"Max Memory Used: %s MB\t\n",
		invokeId, invokeDuration, math.Ceil(invokeDuration), memorySize, memorySize)
}

type AwsFunctionRequestContext struct {
	DomainName   string            `json:"domainName"`
	DomainPrefix string            `json:"domainPrefix"`
	Http         map[string]string `json:"http"`
}

type AwsFunctionRequestPayload struct {
	Method                string                    `json:"method"`
	RawPath               string                    `json:"rawPath"`
	RawQueryString        string                    `json:"rawQueryString"`
	QueryStringParameters map[string]string         `json:"queryStringParameters"`
	Headers               map[string]string         `json:"headers"`
	RequestContext        AwsFunctionRequestContext `json:"requestContext"`
	Body                  string                    `json:"body"`
	IsBase64Encoded       bool                      `json:"isBase64Encoded"`
}

// invoke lambda function in function-url style
// see https://docs.aws.amazon.com/lambda/latest/dg/urls-invocation.html
// When a client calls your function URL, Lambda maps the request to an event object before passing it to your function.
func DirectInvokeHandler(w http.ResponseWriter, r *http.Request, sandbox Sandbox, bs interop.Bootstrap) {
	// the `DirectInvokeHandler` simply maps request to event object and pass it to `InvokeHandler`

	log.Debugf("invoke: -> %s %s %v", r.Method, r.URL, r.Header)
	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("Failed to read invoke body: %s", err)
		w.WriteHeader(500)
		return
	}

	rawPath := "/" + chi.URLParam(r, "*")

	ctx := AwsFunctionRequestContext{
		DomainName: r.Host,
		Http:       map[string]string{},
	}
	ctx.Http["method"] = r.Method
	ctx.Http["path"] = rawPath
	host_split := strings.Split(r.Host, ".")
	if len(host_split) > 1 {
		ctx.DomainPrefix = host_split[0]
	}

	proxy_req := AwsFunctionRequestPayload{
		Method:                r.Method,
		RawPath:               rawPath,
		RawQueryString:        r.URL.RawQuery,
		QueryStringParameters: map[string]string{},
		RequestContext:        ctx,
		Headers:               map[string]string{},
		Body:                  base64.StdEncoding.EncodeToString(bodyBytes),
		IsBase64Encoded:       true,
	}

	for k, vs := range r.URL.Query() {
		proxy_req.QueryStringParameters[k] = strings.Join(vs, ",")
	}

	for k, vs := range r.Header {
		proxy_req.Headers[k] = strings.Join(vs, ",")
	}

	bodyBytes, err = json.Marshal(proxy_req)
	if err != nil {
		log.Errorf("Failed json.Marshal proxy_req: %s", err)
		w.WriteHeader(500)
		return
	}

	var buf bytes.Buffer
	buf.Write(bodyBytes)
	r.Body = io.NopCloser(io.Reader(&buf))
	r.Header.Set("Content-Length", fmt.Sprint(len(bodyBytes)))

	InvokeHandler(w, r, sandbox, bs)
}

func InvokeHandler(w http.ResponseWriter, r *http.Request, sandbox Sandbox, bs interop.Bootstrap) {
	log.Debugf("invoke: -> %s %s %v", r.Method, r.URL, r.Header)
	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("Failed to read invoke body: %s", err)
		w.WriteHeader(500)
		return
	}

	initDuration := ""
	inv := GetenvWithDefault("AWS_LAMBDA_FUNCTION_TIMEOUT", "300")
	timeoutDuration, _ := time.ParseDuration(inv + "s")
	// Default
	timeout, err := strconv.ParseInt(inv, 10, 64)
	if err != nil {
		panic(err)
	}

	functionVersion := GetenvWithDefault("AWS_LAMBDA_FUNCTION_VERSION", "$LATEST")
	memorySize := GetenvWithDefault("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "3008")

	if !initDone {

		initStart, initEnd := InitHandler(sandbox, functionVersion, timeout, bs)

		// Calculate InitDuration
		initTimeMS := math.Min(float64(initEnd.Sub(initStart).Nanoseconds()),
			float64(timeoutDuration.Nanoseconds())) / float64(time.Millisecond)

		initDuration = fmt.Sprintf("Init Duration: %.2f ms\t", initTimeMS)

		// Set initDone so next invokes do not try to Init the function again
		initDone = true
	}

	invokeStart := time.Now()
	invokePayload := &interop.Invoke{
		ID:                 uuid.New().String(),
		InvokedFunctionArn: fmt.Sprintf("arn:aws:lambda:us-east-1:012345678912:function:%s", GetenvWithDefault("AWS_LAMBDA_FUNCTION_NAME", "test_function")),
		TraceID:            r.Header.Get("X-Amzn-Trace-Id"),
		LambdaSegmentID:    r.Header.Get("X-Amzn-Segment-Id"),
		Payload:            bytes.NewReader(bodyBytes),
	}
	fmt.Println("START RequestId: " + invokePayload.ID + " Version: " + functionVersion)

	// If we write to 'w' directly and waitUntilRelease fails, we won't be able to propagate error anymore
	invokeResp := &ResponseWriterProxy{}
	if err := sandbox.Invoke(invokeResp, invokePayload); err != nil {
		switch err {

		// Reserve errors:
		case rapidcore.ErrAlreadyReserved:
			log.Errorf("Failed to reserve: %s", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		case rapidcore.ErrInternalServerError:
			w.WriteHeader(http.StatusInternalServerError)
			return
		case rapidcore.ErrInitDoneFailed:
			w.WriteHeader(http.StatusBadGateway)
			w.Write(invokeResp.Body)
			return
		case rapidcore.ErrReserveReservationDone:
			// TODO use http.StatusBadGateway
			w.WriteHeader(http.StatusGatewayTimeout)
			return

		// Invoke errors:
		case rapidcore.ErrNotReserved:
		case rapidcore.ErrAlreadyReplied:
		case rapidcore.ErrAlreadyInvocating:
			log.Errorf("Failed to set reply stream: %s", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		case rapidcore.ErrInvokeReservationDone:
			// TODO use http.StatusBadGateway
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		case rapidcore.ErrInvokeResponseAlreadyWritten:
			return
		// AwaitRelease errors:
		case rapidcore.ErrInvokeDoneFailed:
			w.WriteHeader(http.StatusBadGateway)
			w.Write(invokeResp.Body)
			return
		case rapidcore.ErrReleaseReservationDone:
			// TODO return sandbox status when we implement async reset handling
			// TODO use http.StatusOK
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		case rapidcore.ErrInvokeTimeout:
			printEndReports(invokePayload.ID, initDuration, memorySize, invokeStart, timeoutDuration)

			w.Write([]byte(fmt.Sprintf("Task timed out after %d.00 seconds", timeout)))
			time.Sleep(100 * time.Millisecond)
			//initDone = false
			return
		}
	}

	printEndReports(invokePayload.ID, initDuration, memorySize, invokeStart, timeoutDuration)

	if invokeResp.StatusCode != 0 {
		w.WriteHeader(invokeResp.StatusCode)
	}
	w.Write(invokeResp.Body)
}

func InitHandler(sandbox Sandbox, functionVersion string, timeout int64, bs interop.Bootstrap) (time.Time, time.Time) {
	additionalFunctionEnvironmentVariables := map[string]string{}

	// Add default Env Vars if they were not defined. This is a required otherwise 1p Python2.7, Python3.6, and
	// possibly others pre runtime API runtimes will fail. This will be overwritten if they are defined on the system.
	additionalFunctionEnvironmentVariables["AWS_LAMBDA_LOG_GROUP_NAME"] = "/aws/lambda/Functions"
	additionalFunctionEnvironmentVariables["AWS_LAMBDA_LOG_STREAM_NAME"] = "$LATEST"
	additionalFunctionEnvironmentVariables["AWS_LAMBDA_FUNCTION_VERSION"] = "$LATEST"
	additionalFunctionEnvironmentVariables["AWS_LAMBDA_FUNCTION_MEMORY_SIZE"] = "3008"
	additionalFunctionEnvironmentVariables["AWS_LAMBDA_FUNCTION_NAME"] = "test_function"

	// Forward Env Vars from the running system (container) to what the function can view. Without this, Env Vars will
	// not be viewable when the function runs.
	for _, env := range os.Environ() {
		// Split the env into by the first "=". This will account for if the env var's value has a '=' in it
		envVar := strings.SplitN(env, "=", 2)
		additionalFunctionEnvironmentVariables[envVar[0]] = envVar[1]
	}

	initStart := time.Now()
	// pass to rapid
	sandbox.Init(&interop.Init{
		Handler:           GetenvWithDefault("AWS_LAMBDA_FUNCTION_HANDLER", os.Getenv("_HANDLER")),
		AwsKey:            os.Getenv("AWS_ACCESS_KEY_ID"),
		AwsSecret:         os.Getenv("AWS_SECRET_ACCESS_KEY"),
		AwsSession:        os.Getenv("AWS_SESSION_TOKEN"),
		XRayDaemonAddress: "0.0.0.0:0", // TODO
		FunctionName:      GetenvWithDefault("AWS_LAMBDA_FUNCTION_NAME", "test_function"),
		FunctionVersion:   functionVersion,
		RuntimeInfo: interop.RuntimeInfo{
			ImageJSON: "{}",
			Arn:       "",
			Version:   ""},
		CustomerEnvironmentVariables: additionalFunctionEnvironmentVariables,
		SandboxType:                  interop.SandboxClassic,
		Bootstrap:                    bs,
		EnvironmentVariables:         env.NewEnvironment(),
	}, timeout*1000)
	initEnd := time.Now()
	return initStart, initEnd
}
