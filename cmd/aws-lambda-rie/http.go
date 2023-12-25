// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"

	"github.com/go-chi/chi"
	log "github.com/sirupsen/logrus"
	"go.amzn.com/lambda/interop"
	"go.amzn.com/lambda/rapidcore"
)

func startHTTPServer(ipport string, sandbox *rapidcore.SandboxBuilder, bs interop.Bootstrap) {

	r := chi.NewRouter()
	r.Post("/2015-03-31/functions/function/invocations", func(w http.ResponseWriter, r *http.Request) { InvokeHandler(w, r, sandbox.LambdaInvokeAPI(), bs) })
	r.Post("/*", func(w http.ResponseWriter, r *http.Request) { DirectInvokeHandler(w, r, sandbox.LambdaInvokeAPI(), bs) })

	if err := http.ListenAndServe(ipport, r); err != nil {
		log.Panic(err)
	}

	log.Warnf("Listening on %s", ipport)
}
