# pricing-probe

Manual diagnostic that compares price sources and request options from the machine it runs on
(e.g. the homelab). It is a separate `main` package: it is not built into the service image and
never runs in the cluster. No secrets are needed.

## Run

    go run ./tools/pricing-probe
    go run ./tools/pricing-probe -items "milk,pasta" -delay 5s -max-requests 20 -out report.json

Flags: `-items` (comma-separated), `-delay` (pause between requests, default 3s), `-max-requests`
(hard cap, default 30), `-out` (JSON report path), `-timeout`, and `-tesco-url` / `-off-url` /
`-prices-url` to point at other base URLs.

## What it probes

- **tesco/honest-ua** and **tesco/browser-headers**: the two header profiles already used by the
  service. Nothing else is tried (no proxies, spoofing or CAPTCHA handling).
- **tesco/robots.txt**: fetched once, reported as `info`.
- **open-food-facts/search**: product search; it usually has no shelf prices, so expect `empty`.
- **open-prices/prices-by-name**: community-submitted prices from Open Prices.

## Reading results

The matrix has one row per source/option and one column per item; each cell is
`status classification size latency`. Classifications:

- `ok`: HTTP 200 and at least one price parsed
- `blocked`: 403/429 or a bot-challenge page
- `empty`: HTTP 200 but no usable price
- `error`: transport error or unexpected status
- `info`: informational fetch (robots.txt)

The JSON report holds the same data per request (URL, status, size, latency, `price_parsed`,
price count, note). If the request cap is reached the run stops and `truncated` is true.
