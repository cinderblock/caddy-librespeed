# caddy-librespeed

A [Caddy](https://caddyserver.com) module that serves a speed test: the
[LibreSpeed](https://github.com/librespeed/speedtest) backend protocol, plus a
built-in browser UI. No PHP, no separate server, and no files on disk.

```caddyfile
speedtest.example.com {
	librespeed
}
```

That's the whole site. Open it in a browser and press Start.

## Why

LibreSpeed's reference backend is PHP. Each transfer ties up a PHP worker for
its whole duration, and every download request generates a fresh random megabyte.
This module answers the same protocol from inside Caddy instead:

- Downloads come from one random 1 MiB buffer, generated at startup and written
  as many times as the client asks for.
- Uploads are read and discarded.
- Nothing else is needed beyond the Caddy binary.

Clients that speak LibreSpeed's protocol work unchanged; for example,
[`librespeed-cli`](https://github.com/librespeed/speedtest-cli) with its usual
`backend/garbage.php` server entries.

## Install

```sh
xcaddy build --with github.com/cinderblock/caddy-librespeed
```

## Endpoints

The handler matches the **last path segment**, with or without `.php`, so
it works under any prefix. For example, LibreSpeed's `backend/garbage.php`
layout works unchanged.

| Segment                    | Does                                                                     |
| -------------------------- | ------------------------------------------------------------------------ |
| `garbage` / `garbage.php`  | `GET ?ckSize=N` streams N MiB of random data (default 4, at most 1024)    |
| `empty` / `empty.php`      | Reads and discards the request body. Used for the upload test and pings |
| `getIP` / `getIP.php`      | `{"processedString":"<your IP>","rawIspInfo":""}`                        |
| `config.json`              | The UI's server list                                                     |
| anything else              | The built-in UI (`…/` serves `index.html`), or 404                       |

Notes:

- Add `?cors` to any endpoint to get CORS headers, as with LibreSpeed. The
  built-in UI always sends it.
- Responses are never cached.
- Downloads carry `Cache-Control: no-transform`, so a stray `encode`
  directive (or a CDN) won't compress the test data.
- `getIP` reports Caddy's `client_ip`, so it honours your `trusted_proxies`
  setting. It never looks up ISP details from a third party.

## Options

```caddyfile
librespeed [<matcher>] {
	max_upload_size <size>     # per request; default 64MiB
	ui on|off                  # the built-in UI; default on
	server <name> <url>        # repeatable; see below
}
```

`server` adds an entry to the UI's server menu. The UI pings each server when
the page loads, so you can pick the closest. Each URL must be absolute and end
with `/`. Other servers must also run this module (or another LibreSpeed
backend that honours `?cors`). With no `server` lines, the UI tests the server
it was loaded from.

```caddyfile
(speedtest) {
	librespeed {
		server "San Francisco" https://speedtest.sf.example.com/
		server "Helsinki"      https://speedtest.hel.example.com/
	}
}

speedtest.sf.example.com {
	import speedtest
}
```

### Under a path prefix

```caddyfile
example.com {
	redir /speedtest /speedtest/
	handle /speedtest/* {
		librespeed
	}
}
```

The UI uses relative URLs, so it needs the trailing slash, which is what the
`redir` adds.

## Measuring

The UI runs three tests in order:

1. **Ping and jitter.** Ten sequential requests to `empty`, after one warm-up
   request. It uses Resource Timing where available.
2. **Download.** Six parallel streams for 13 s.
3. **Upload.** Three parallel streams of 20 MiB blobs for 13 s.

The first 3 s of each throughput test is a warm-up and isn't counted: TCP
slow start and send buffers distort it. The reported speed is the average
over the remaining 10 s. That is goodput (payload bytes per second), with no
overhead fudge factor.

## Development

```sh
go test ./...                     # unit tests, plus a real Caddy from a Caddyfile
go test -run '^$' -bench .        # handler benchmarks
xcaddy build --with github.com/cinderblock/caddy-librespeed=.
```

The tests keep Caddy's state in a temporary directory and never touch your
real Caddy config or certificate storage.

The UI is plain HTML, CSS and JavaScript modules in `ui/`, embedded with
`go:embed`, with no build step. `ui/speedtest.js` is the measurement engine
and doesn't touch the DOM.

## License

MIT
