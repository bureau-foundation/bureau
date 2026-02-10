// SPDX-License-Identifier: Apache-2.0
//
// bb-scheduler configuration for Bureau's Buildbarn deployment.
//
// Receives Execute requests forwarded from bb-storage (the frontend),
// queues actions, and dispatches them to bb-worker instances. Workers
// register via the Synchronize RPC on the workerGrpcServers port.
//
// The scheduler is stateless — all queue state is in-memory. Restarting
// the scheduler drops all queued and in-flight actions; workers
// reconnect and re-register automatically.

{
  // Admin web UI for inspecting the build queue: queued, executing,
  // and completed actions. Browse at http://host:7982/.
  adminHttpServers: [{
    listenAddresses: [':7982'],
    authenticationPolicy: { allow: {} },
  }],

  // Client gRPC: receives Execute/WaitExecution RPCs from the
  // bb-storage frontend.
  clientGrpcServers: [{
    listenAddresses: [':8982'],
    authenticationPolicy: { allow: {} },
  }],

  // Worker gRPC: bb-worker instances connect here with the Synchronize
  // RPC to receive work assignments.
  workerGrpcServers: [{
    listenAddresses: [':8983'],
    authenticationPolicy: { allow: {} },
  }],

  // BuildQueueState gRPC: programmatic access to queue state (same
  // data as the admin web UI).
  buildQueueStateGrpcServers: [{
    listenAddresses: [':8984'],
    authenticationPolicy: { allow: {} },
  }],

  // CAS access for reading Action and Command protos from execute
  // requests. The scheduler does not need the action cache.
  contentAddressableStorage: {
    grpc: { address: 'storage:8980' },
  },

  maximumMessageSizeBytes: 16 * 1024 * 1024,

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

  executeAuthorizer: { allow: {} },
  modifyDrainsAuthorizer: { allow: {} },
  killOperationsAuthorizer: { allow: {} },
  synchronizeAuthorizer: { allow: {} },

  // Action routing: extract the platform key from the action proto and
  // use per-invocation keys for fair scheduling across concurrent
  // builds. Workers register with matching platform properties; the
  // scheduler routes actions to workers whose platform matches.
  actionRouter: {
    simple: {
      platformKeyExtractor: { action: {} },
      invocationKeyExtractors: [
        { correlatedInvocationsId: {} },
        { toolInvocationId: {} },
      ],
      initialSizeClassAnalyzer: {
        defaultExecutionTimeout: '1800s',
        maximumExecutionTimeout: '7200s',
      },
    },
  },

  // If no workers are registered for a platform queue for this duration,
  // the queue is garbage collected. 15 minutes allows for transient
  // worker restarts without losing queued actions.
  platformQueueWithNoWorkersTimeout: '900s',
}
