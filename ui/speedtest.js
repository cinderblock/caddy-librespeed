// Measurement engine for a LibreSpeed-protocol server. No DOM access, so it
// can be reused from other pages.
//
// Every function takes `base`, the URL the server's endpoints live under
// (absolute, or relative to this page), ending with "/".

const MiB = 1 << 20;

/** Throughput test tuning. */
export const defaults = {
  download: { streams: 6, duration: 13000, grace: 3000, ckSize: 100 },
  upload: { streams: 3, duration: 13000, grace: 3000, blobMiB: 20 },
  ping: { count: 10 },
  sampleInterval: 250,
};

function endpoint(base, name, params = {}) {
  const u = new URL(name, new URL(base, location.href));
  // Always ask for CORS headers: harmless same-origin, required cross-origin.
  u.searchParams.set('cors', '');
  for (const [k, v] of Object.entries(params)) u.searchParams.set(k, v);
  // Defeats any cache that ignores Cache-Control.
  u.searchParams.set('r', crypto.getRandomValues(new Uint32Array(2)).join('-'));
  return u.href;
}

async function check(res, what) {
  if (!res.ok) throw new Error(`${what}: HTTP ${res.status} ${res.statusText}`);
  return res;
}

/**
 * The client's IP as the server sees it.
 * @returns {Promise<string>}
 */
export async function getIP(base, { signal } = {}) {
  const res = await check(await fetch(endpoint(base, 'getIP'), { cache: 'no-store', signal }), 'getIP');
  const { processedString } = await res.json();
  return processedString;
}

/**
 * Round-trip latency. One warm-up request opens the connection and is not
 * counted. Uses Resource Timing when the browser exposes it (request start
 * to first response byte), which excludes the page's own scheduling delays.
 * @returns {Promise<{ping: number, jitter: number, samples: number[]}>} milliseconds
 */
export async function ping(base, { count = defaults.ping.count, signal, onSample } = {}) {
  const samples = [];
  for (let i = 0; i <= count; i++) {
    const href = endpoint(base, 'empty');
    const t0 = performance.now();
    const res = await check(await fetch(href, { cache: 'no-store', signal }), 'ping');
    await res.arrayBuffer();
    let ms = performance.now() - t0;
    const [entry] = performance.getEntriesByName(href);
    if (entry && entry.requestStart > 0 && entry.responseStart > entry.requestStart) {
      ms = entry.responseStart - entry.requestStart;
    }
    performance.clearResourceTimings();
    if (i === 0) continue;
    samples.push(ms);
    onSample?.(ms);
  }
  const sorted = [...samples].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  const median = sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
  let jitter = 0;
  for (let i = 1; i < samples.length; i++) jitter += Math.abs(samples[i] - samples[i - 1]);
  jitter /= Math.max(1, samples.length - 1);
  return { ping: median, jitter, samples };
}

/**
 * Runs `streams` copies of `stream(meter, signal)` for `duration` ms and
 * measures their combined throughput. Bytes moved during the first `grace`
 * ms (TCP slow start, buffers filling) are not counted.
 *
 * onProgress receives {t, bps, average, counting}: elapsed ms, throughput over
 * the last sample interval, and the counted average so far (null during grace).
 * @returns {Promise<{bps: number, samples: {t: number, bps: number}[]}>}
 */
async function throughput({ streams, duration, grace, stream, signal, onProgress }) {
  const phase = new AbortController();
  const stop = () => phase.abort();
  signal?.addEventListener('abort', stop, { once: true });

  const meter = { bytes: 0 };
  const start = performance.now();
  let counted = null; // {t, bytes} at the end of the grace period
  let last = { t: 0, bytes: 0 };
  const samples = [];

  const sample = (final = false) => {
    const t = performance.now() - start;
    if (!counted && t >= grace) counted = { t, bytes: meter.bytes };
    const average = counted && t > counted.t ? ((meter.bytes - counted.bytes) * 8 * 1000) / (t - counted.t) : null;
    // The final call lands at an arbitrary moment after the last tick; too
    // short an interval to be a meaningful sample on its own.
    if (!final || t - last.t >= defaults.sampleInterval / 2) {
      const bps = ((meter.bytes - last.bytes) * 8 * 1000) / (t - last.t);
      last = { t, bytes: meter.bytes };
      samples.push({ t, bps });
      onProgress?.({ t, bps, average, counting: !!counted });
    }
    return average;
  };

  let failure = null;
  const workers = Array.from({ length: streams }, () =>
    stream(meter, phase.signal).catch((err) => {
      // Our own abort is how every stream ends; anything else is a real failure.
      if (!phase.signal.aborted) {
        failure ??= err;
        phase.abort();
      }
    }),
  );

  const timer = setInterval(sample, defaults.sampleInterval);
  const ended = setTimeout(stop, duration);
  await new Promise((resolve) => phase.signal.addEventListener('abort', resolve, { once: true }));
  clearInterval(timer);
  clearTimeout(ended);
  signal?.removeEventListener('abort', stop);
  const average = sample(true);
  await Promise.all(workers);

  if (failure) throw failure;
  signal?.throwIfAborted();
  if (average === null) throw new Error('the test ended before the warm-up period did');
  return { bps: average, samples };
}

/** Download throughput, in bits per second. */
export function download(base, { signal, onProgress, ...opts } = {}) {
  const o = { ...defaults.download, ...opts };
  return throughput({
    ...o,
    signal,
    onProgress,
    async stream(meter, sig) {
      for (;;) {
        const res = await check(
          await fetch(endpoint(base, 'garbage', { ckSize: o.ckSize }), { cache: 'no-store', signal: sig }),
          'download',
        );
        const reader = res.body.getReader();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          meter.bytes += value.byteLength;
        }
      }
    },
  });
}

let payload = null;

/** One random MiB, repeated: incompressible, and cheap to build. */
function uploadBlob(mib) {
  if (!payload) {
    payload = new Uint8Array(MiB);
    for (let i = 0; i < MiB; i += 65536) crypto.getRandomValues(payload.subarray(i, i + 65536));
  }
  return new Blob(Array(mib).fill(payload), { type: 'application/octet-stream' });
}

// fetch() can't report upload progress, so uploads use XMLHttpRequest.
function post(href, blob, meter, signal) {
  return new Promise((resolve, reject) => {
    signal.throwIfAborted();
    const xhr = new XMLHttpRequest();
    let sent = 0;
    const abort = () => xhr.abort();
    const done = (fn) => (arg) => {
      signal.removeEventListener('abort', abort);
      fn(arg);
    };
    xhr.upload.onprogress = (e) => {
      meter.bytes += e.loaded - sent;
      sent = e.loaded;
    };
    xhr.onload = done(() => {
      if (xhr.status !== 200) return reject(new Error(`upload: HTTP ${xhr.status} ${xhr.statusText}`));
      meter.bytes += blob.size - sent;
      resolve();
    });
    xhr.onerror = done(() => reject(new Error('upload: network error')));
    xhr.onabort = done(() => reject(new DOMException('upload aborted', 'AbortError')));
    signal.addEventListener('abort', abort, { once: true });
    xhr.open('POST', href);
    xhr.send(blob);
  });
}

/** Upload throughput, in bits per second. */
export function upload(base, { signal, onProgress, ...opts } = {}) {
  const o = { ...defaults.upload, ...opts };
  const blob = uploadBlob(o.blobMiB);
  return throughput({
    ...o,
    signal,
    onProgress,
    async stream(meter, sig) {
      for (;;) await post(endpoint(base, 'empty'), blob, meter, sig);
    },
  });
}
