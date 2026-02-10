// SPDX-License-Identifier: Apache-2.0
//
// bb-worker configuration for Bureau's Buildbarn deployment.
//
// Connects to bb-scheduler to receive work assignments via Synchronize
// RPC, and to bb-storage for CAS access (downloading action inputs,
// uploading outputs).
//
// Uses the native/hardlinking build directory: the worker downloads all
// action inputs from CAS and hardlinks them into the build directory
// before telling the runner to execute. A file cache avoids
// re-downloading inputs that appear across multiple actions.
//
// The runner executes build commands in an isolated container connected
// via a unix socket at /worker/runner.

{
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

  maximumMessageSizeBytes: 16 * 1024 * 1024,

  scheduler: { address: 'scheduler:8983' },

  // CAS and AC access for downloading action inputs and uploading outputs.
  // The action cache uses completeness checking: before returning an AC
  // hit, the worker verifies that all referenced CAS blobs still exist.
  // This prevents serving stale results when CAS has garbage-collected
  // blobs that an AC entry references.
  contentAddressableStorage: {
    grpc: { address: 'storage:8980' },
  },
  actionCache: {
    completenessChecking: {
      backend: {
        grpc: { address: 'storage:8980' },
      },
      maximumTotalTreeSizeBytes: 64 * 1024 * 1024,
    },
  },

  buildDirectories: [{
    native: {
      buildDirectoryPath: '/worker/build',
      cacheDirectoryPath: '/worker/cache',
      maximumCacheFileCount: 10000,
      maximumCacheSizeBytes: 1 * 1024 * 1024 * 1024,
      cacheReplacementPolicy: 'LEAST_RECENTLY_USED',
    },
    runners: [{
      endpoint: { address: 'unix:///worker/runner' },

      // How many actions this worker executes simultaneously. This is
      // the primary parallelism knob. Size to roughly (total cores - 4)
      // to leave headroom for infrastructure containers (storage,
      // scheduler, worker I/O). Each Go compile action uses ~1 core and
      // 200-500 MB RAM.
      concurrency: 8,

      // Accept all instance name prefixes. Different projects sharing
      // this cluster can use any --remote_instance_name value.
      instanceNamePrefix: '',

      // Platform properties must match exactly what Bazel sends via the
      // execution platform's exec_properties. The scheduler routes
      // actions to workers whose platform set is an exact match.
      platform: {
        properties: [
          { name: 'OSFamily', value: 'linux' },
        ],
      },

      workerId: {
        hostname: 'bureau-worker',
      },
    }],
  }],

  inputDownloadConcurrency: 10,
  outputUploadConcurrency: 11,
}
