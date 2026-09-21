# Production calendar

The month view shows recording, editing, and release milestones, with a named month/year heading, weekday headings, weekend shading, and a current-day highlight. Phones show a compact month grid followed by a dated agenda with episode links and holiday names. Month navigation supports 2000 through 2100.

The optional U.S. holiday overlay includes the recurring nationwide federal schedule and these common observances: Valentine's Day, St. Patrick's Day, Gregorian Easter, Mother's Day, Father's Day, Halloween, Election Day in even years, Black Friday, Christmas Eve, and New Year's Eve. Weekend federal holidays show both their actual date and their observed weekday, including New Year's Day observed in the preceding December. Juneteenth begins in 2021. Dates are computed locally without a holiday service or network request.

Federal recurrence and observation rules follow the [OPM holiday schedule](https://www.opm.gov/policy-data-oversight/pay-leave/federal-holidays/). Gregorian Easter test dates are checked against the [Census Bureau date table](https://www.census.gov/data/software/x13as/genhol/easter-dates.html). Regional holidays and one-off government closures are outside this overlay.

`node --test web/calendar_test.mjs` checks dates, observed days, leap-month navigation, and year boundaries. The browser workflow checks month headings, holiday visibility, leap days, and layouts from 320 to 1440 pixels, including a minimum gap between research toolbar controls.

## Calendar subscriptions

Open **Calendar sync** from the production calendar or Profile. Create a private subscription URL and copy it before leaving the page. The database stores only its SHA-256 hash. The same URL works in multiple clients; replacing it invalidates the previous URL, and revoking it disables further downloads. Restoring a backup revokes all calendar subscriptions to prevent old links from becoming valid again. Create a new link, remove the old subscribed calendar, and add the new URL after a restore. Clients may retain events from the old calendar.

- Google Calendar on a computer: **Other calendars + → From URL**, paste the URL, then **Add calendar** ([Google instructions](https://support.google.com/calendar/answer/37100)).
- Apple Calendar on Mac: **File → New Calendar Subscription**, paste the URL, and choose the **iCloud** account for availability on other devices ([Apple instructions](https://support.apple.com/guide/calendar/subscribe-to-calendars-icl1022/mac)).

This is a read-only iCalendar subscription. Edit dates in Theses. Refresh timing is controlled by the calendar client and can be delayed; importing a downloaded file does not subscribe to future changes. The server must be reachable from the calendar provider. Holiday overlays are not included, so clients can use their own holiday calendars.

Each refresh includes recording, editing and release dates for active episodes the subscribing user can currently access. It contains episode titles, all-day dates and links back to Theses, without notes or scripts. Anyone holding the URL can fetch those details without signing in. Membership removal, role changes and account deletion affect subsequent requests; previously downloaded data can remain in a provider's cache.

The feed follows [RFC 5545](https://www.rfc-editor.org/rfc/rfc5545): stable event identifiers, durable revision counters, UTC modification stamps, escaped text, UTF-8-safe line folding and exclusive end dates. Subscription mutations use the existing CSRF and audit transaction paths. Feed URLs are redacted from application request logs; reverse proxies should also avoid logging credential-bearing paths.
