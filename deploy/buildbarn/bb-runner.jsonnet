// SPDX-License-Identifier: Apache-2.0
//
// bb-runner configuration for Bureau's Buildbarn deployment.
//
// The runner is deliberately thin: it receives gRPC requests from
// bb-worker, executes the build command in the shared build directory,
// and returns the result. All CAS I/O is handled by the worker.
//
// The runner communicates with the worker via a unix socket at
// /worker/runner. The build directory at /worker/build is shared
// between worker and runner via a Docker volume.

{
  buildDirectoryPath: '/worker/build',

  grpcServers: [{
    listenPaths: ['/worker/runner'],
    authenticationPolicy: { allow: {} },
  }],

  global: {
    diagnosticsHttpServer: {
      httpServers: [{
        listenAddresses: [':9980'],
        authenticationPolicy: { allow: {} },
      }],
      enablePrometheus: true,
      enablePprof: true,
      enableActiveSpans: true,
    },
  },
}
