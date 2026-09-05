# V1 Implementation Review Findings

## Context

Two independent implementation reviews were completed after the V1 implementation.

This document consolidates implementation findings that can be established from the current code and the V1 design materials.

Protocol behaviors that still require a real WeRead account are recorded separately in `protocol-validation-gaps.md`.

## Findings

### F1. Daemon and manual `run` do not share a task lock

Locations:

- `internal/app/app.go`
- daemon execution path
- `weread-cron run`

The application uses an in-process `sync.Mutex` to prevent concurrent Tasks.

A normal manual invocation in Docker starts another operating-system process:

    docker compose exec weread-cron /weread-cron run

The new process has its own mutex.

A running Task does not create a Terminal State until it finishes. Therefore, the second process can also pass the Terminal State gate and start another Task.

This permits two Tasks for the same deployment to run at the same time.

The possible effects include concurrent renewal, concurrent Reading Sessions, duplicate Timed reports, and concurrent writes to Login Session and Terminal State files.

The V1 design states that a second Task must be rejected while a Task is already running.


### F2. A transient Task failure can end the day without a Terminal State or failure notification

Locations:

- `internal/task/task.go`
- `internal/scheduler/scheduler.go`

A transport error or another failure that is not classified as a final rejection can return from the current Task without writing a failed Terminal State.

The daemon treats this as a transient failure and attempts to schedule another Task in the current Run Window.

If no Run Window time remains, the scheduler moves directly to the next day.

The resulting day can therefore have all of these conditions:

- no successful Terminal State;
- no failed Terminal State;
- no further Task attempt;
- no failure notification.

The V1 model requires a final failed automatic Task to produce a failure notification and allow later manual recovery with `weread-cron run`.

A related implementation detail is that a transport-level failure in a Timed report currently ends the current Task attempt immediately unless it is an explicit report rejection.


### F3. Automatic Shelf selection is not random

Location:

- `internal/task/task.go`

When no candidate book IDs are configured, Shelf books are divided into preferred tiers and probed in list order.

The first usable book in the preferred tier is selected.

The candidate list is not randomized before probing.

As a result, a stable Shelf response can cause the same first usable unread book to be selected every day.

The V1 design specifies random selection among eligible candidates, with unread books preferred over completed books.


### F4. Explicit zero Reading Progress values can be treated as missing

Locations:

- `internal/weread/readerstate.go`
- `internal/weread/readerstate_test.go`

Reading Progress fallback currently uses numeric zero to decide whether a value from `currentChapter` is unavailable.

Values such as these are valid:

    chapterIdx = 0
    chapterOffset = 0

The parser does not separately record whether the field exists.

A valid explicit zero can therefore be replaced by a fallback value from another progress object.

The current tests cover non-zero precedence and missing values, but do not cover an explicit zero that differs from the fallback value.


### F5. Failed Terminal State persistence is not a strict prerequisite for failure notification

Location:

- `internal/task/task.go`

The success path requires Terminal State persistence to succeed before success notification is sent.

Some final failure paths behave differently:

    attempt failed Terminal State write
    → log the write error
    → continue with failure notification

If Terminal State persistence fails, the user can receive a final failure notification while the durable state still contains no final result for that day.

A later daemon execution can therefore observe no Terminal State and attempt another Task.

The V1 design records Terminal State before notification so that a process restart cannot cause an already-final Task to run again.


### F6. Response Cookies can be persisted before the response is accepted

Location:

- `internal/weread/client.go`

The common HTTP response path currently merges response Cookies before it verifies the HTTP status and before the caller verifies the business response.

For renewal, the effective order is:

    receive response
    → merge and persist Set-Cookie
    → check HTTP status
    → parse and validate renewal success

A failed response can therefore change the persisted Login Session before the response has been accepted as a successful renewal.

This differs from the transactional expectation that an unsuccessful renewal leaves the last known Login Session unchanged.


### F7. A suspended daemon can start a scheduled Task after the Run Window

Location:

- `internal/scheduler/scheduler.go`

The daemon calculates a random start time inside the Run Window and sleeps until that time.

After the wait returns, it checks the Terminal State again but does not verify that the current time is still inside the allowed start window.

If the host sleeps while the daemon is waiting and resumes after the Run Window has ended, the previously scheduled Task can start outside the Run Window.

The V1 definition states that the Run Window constrains Task start time.


### F8. Timed-report waiting does not react promptly to cancellation

Locations:

- `internal/task/task.go`
- `internal/clock/clock.go`

The active Reading Session waits for the next report with a Clock sleep operation.

In production this becomes a normal time sleep and does not observe `context.Context` cancellation during the wait.

A shutdown signal received during this sleep can therefore wait until the report interval finishes before the Task exits.

The daemon scheduler has shorter cancellation-aware waiting behavior, so the two paths have different shutdown responsiveness.


### F9. Notification transport errors can expose notification credentials in logs

Location:

- `internal/notify/notify.go`

Bark credentials are part of the request URL path.

The WeCom robot key is part of the request URL query.

Transport-level HTTP errors can include the complete request URL in the returned Go error.

These errors are wrapped and later written to logs.

A DNS, TLS, connection, or similar transport error can therefore expose a Bark key or WeCom webhook key in container logs.

Normal HTTP status-error handling does not have the same URL exposure.