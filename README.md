# ratecheck

Finds out whether a rate limiter actually holds when requests arrive together.

[![CI](https://github.com/Nappuccino-tlg/ratecheck/actions/workflows/ci.yml/badge.svg)](https://github.com/Nappuccino-tlg/ratecheck/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/go-1.25%20--%201.27-blue)
![License](https://img.shields.io/badge/license-MIT-green)
![Dependencies](https://img.shields.io/badge/dependencies-none-brightgreen)

```bash
go install github.com/Nappuccino-tlg/ratecheck/cmd/ratecheck@latest
```

Or take a binary from [releases](https://github.com/Nappuccino-tlg/ratecheck/releases).

## The limiter that passes every test and does not work

```python
current = await redis.get(key)
if int(current or 0) >= limit:
    raise TooManyRequests
await redis.incr(key)
```

Send requests one at a time and this is perfect. It allows exactly the limit and rejects
the next one, every time, in every test anybody writes for it.

Send them together and every request in the batch runs `get` before any of them runs
`incr`. They all read the same number, they all see room, and they are all allowed. The
limit was twenty; the batch was fifty; fifty got through.

Nothing detects this. The unit tests pass, the integration tests pass, the code review
passes — reading it, the bug is invisible, because the bug is not in the code, it is in the
gap between the two lines. It surfaces in production, as a bill, or as the thing that was
supposed to stop the credential stuffing and did not.

`ratecheck` sends the batch.

## What it looks like

```console
$ ratecheck https://api.example.com/v1/things -H "Authorization: Bearer $TOKEN"
Finding the limit, one request at a time
  20 allowed, then 429
  firing 30 at once, half again over the limit
  warming 30 connections against the closed window
Waiting for the quota to come back
  quota returned after 10.011s
Releasing 30 requests at once

  Limit        20 requests, then rejected (93ms)
  Headers      limit 20
  Retry-After  10
  Window       quota returned after 10.011s
  Burst        30 at once, 30 allowed, 0 rejected
  Overlap      all 30 hit the wire within 1.2ms, 30 on a warm connection

  LEAKED  A limit of 20 let 30 requests through when they arrived together.

          This is what a check-then-increment limiter looks like from
          outside. Every request in the burst read the same counter before
          any of them wrote it back, so they all saw room. Under real load
          the effective limit is not the configured one, it is however many
          requests happen to overlap.

          The fix is to make the read and the write one operation the server
          cannot interleave: INCR with an expiry, or a Lua script, or a
          single UPDATE ... RETURNING. Not a GET followed by a SET.
```

The same run against the same limit, counted atomically:

```
  Burst        30 at once, 19 allowed, 11 rejected
  Overlap      all 30 hit the wire within 0.6ms, 30 on a warm connection

  HELD    30 at once, 19 allowed, and the budget was 19.
```

Every finding names the fix. A tool that reports a problem and leaves the answer as an
exercise gets silenced by the first person in a hurry.

## The line that makes it a real test

```
Overlap      all 30 hit the wire within 0.6ms, 30 on a warm connection
```

Most concurrency tests are theatre, and this is the line that says whether this one was.

Thirty requests fired from a cold client do not arrive together. Each one opens a socket
and negotiates TLS first — tens to hundreds of milliseconds, different for every
connection — so "thirty at once" reaches the server spread over half a second. A limiter
with a race in it has all the time in the world to serialise, and answers correctly. The
test passes and has proved nothing.

So ratecheck opens the connections first, while the window is still closed and rejections
are free, and only then releases the burst against a barrier. Then it measures what
actually happened: the gap between the first and last request reaching the wire, taken from
Go's own HTTP tracing rather than from when they were queued.

If that spread is too wide to catch a race, the verdict is `unknown` and it says so.
A confident wrong answer would be worse than no tool.

## What it does

1. **Finds the limit** by sending requests one at a time until one is rejected. Whatever
   the API says about itself in `X-RateLimit-*` and `Retry-After` is reported next to the
   number that was actually measured — those disagree more often than you would like.
2. **Waits for the quota to come back**, polling one request at a time, because every
   probe that succeeds is quota the burst will not have.
3. **Releases the burst** the moment the window reopens — the furthest point from the next
   rollover, and the only moment when the whole allowance is there to be exceeded.

The window's shape falls out of the third step rather than costing a phase of its own: how
many the burst was allowed *is* how much had come back. The whole allowance at once is a
fixed window; one request at a time is a sliding window or a token bucket.

## In CI

```yaml
- run: go install github.com/Nappuccino-tlg/ratecheck/cmd/ratecheck@latest
- run: ratecheck -burst 40 "$STAGING/api/things" -H "Authorization: Bearer $TOKEN"
```

| exit | meaning |
|---|---|
| `0` | the limiter held, or there was no limit to test |
| `1` | more got through together than the limit allows |
| `2` | could not tell: bad usage, unreachable host, or a burst too spread out to trust |

`2` is not a pass. It means the question was not answered.

Pass `-burst` in CI. Without it the burst is sized from the limit that was just measured,
which is right for a one-off but makes the run depend on a number that could move.

## Flags

| | |
|---|---|
| `-H "Name: value"` | header to send, repeatable — this is where auth goes |
| `-X` | method, default `GET` |
| `-status` | what "denied" looks like, default `429`. Plenty of APIs answer `403` |
| `-burst` | how many to fire at once. Default: the limit plus half again |
| `-n` | most requests to spend finding the limit, default `200` |
| `-reset` | how long to wait for quota to return, default `90s`. `0` to skip |
| `-pace` | gap between the requests that look for the limit |
| `-timeout` | per-request timeout, default `10s` |
| `-json` | the whole report as JSON, on stdout, with progress suppressed |

## Point it at something you are responsible for

It sends real requests and it deliberately exhausts a quota. On a shared API that means
using up somebody else's allowance; on an endpoint that charges per call it means a bill.
Staging, or your own service, or an account you own.

It is also not a load tester. It stops at the first rejection, fires one burst, and leaves
— the total is a couple of hundred requests, not a couple of hundred thousand.

## What it cannot see

**Per-user limits, from one user.** Everything here runs as one caller. A limiter keyed
per account, tested with one account, tells you about that key and nothing about whether
the limiter shares state correctly across them.

**Distributed leaks.** A burst from one machine goes to one instance behind a load
balancer. A limiter that counts correctly in each process but not across them is a real
and common bug, and this will pass it.

**Whether the limit is the right limit.** It reports what the limiter does, not whether
twenty an hour was a sensible number.

## Why HTTP/1.1

HTTP/2 multiplexes every request onto one connection, so a warm pool of thirty would be
thirty streams sharing a socket, written in whatever order that connection's flow control
allowed. That is not thirty callers arriving at once, which is the thing being measured. A
connection each is the closer model, and the one where the overlap number means something.

## Where this came from

A URL shortener with a rate limiter written the way at the top of this page. It passed its
tests. The way it was eventually caught was a test that opened a pool of connections,
warmed them, and released fifty requests against a barrier — [redlimit](https://github.com/Nappuccino-tlg/redlimit)
is the limiter that replaced it, and that test is its headline.

The test was fifty lines of Python that only worked against that one service. This is that
test, pointed anywhere.

## Requirements

Go 1.25 or newer to build. **No dependencies** — `net/http`, `net/http/httptrace` and
`flag` from the standard library. A tool that drags a dependency tree into somebody's CI is
one more thing for them to resolve, pin and upgrade.

## Tests

```bash
go test -race ./...
```

No services to stand up. The suite runs two HTTP servers of its own — one limiter counting
under a mutex, one reading and writing its counter around a sleep — and checks that
ratecheck calls the second one a leak and does not accuse the first. A tool that cries wolf
on a correct limiter gets uninstalled after one use, so both halves are the test.

## License

MIT
