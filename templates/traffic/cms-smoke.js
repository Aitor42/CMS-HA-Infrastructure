// k6 load/smoke profile for the CMS High-Availability lab.
//
// It is embedded in the orchestrator and run by `cms-ha traffic` inside a
// pinned grafana/k6 container. The entry point and duration are supplied on the
// command line; behaviour is configured through environment variables:
//
//   BASE_URL  Entry point to exercise (keepalived VIP or a CMS frontend).
//   VHOST     HTTP Host header the CMS vhost expects.
//
// TLS verification uses the container trust store; `cms-ha traffic` mounts the
// lab root CA and sets SSL_CERT_FILE when it is present, otherwise it passes
// `--insecure`. Metrics can be streamed to Prometheus with
// `--out experimental-prometheus-rw` (see traffic.prometheus_rw_url).
//
// Run standalone with:
//   BASE_URL=https://192.168.20.100 k6 run templates/traffic/cms-smoke.js

import http from 'k6/http';
import { check, group, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'https://192.168.20.100';
const VHOST = __ENV.VHOST || 'cms.fake-enterprise.com';

// Browse paths mirror the WordPress surface exercised by the old Go generator
// (front page, search, REST API, feeds, cron and the login form).
const BROWSE_PATHS = [
  '/',
  '/wp-login.php',
  '/?s=test',
  '/?s=empresa',
  '/?s=proyecto',
  '/hello-world/',
  '/sample-page/',
  '/wp-cron.php',
  '/wp-json/',
  '/wp-json/wp/v2/posts',
  '/wp-json/wp/v2/pages',
  '/wp-json/wp/v2/users',
  '/feed/',
  '/?s=ha',
];

// Application-level error rate and latency, so dashboards/thresholds are not
// limited to the built-in http_* metrics.
const errorRate = new Rate('cms_errors');
const latency = new Trend('cms_latency', true);

export const options = {
  discardResponseBodies: false,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  thresholds: {
    // A healthy lab answers (almost) everything below 400. 1% headroom absorbs
    // an occasional failover blip without flapping the gate.
    'http_req_failed': ['rate<0.01'],
    'http_req_duration': ['p(95)<2000', 'p(99)<4000'],
    'cms_errors': ['rate<0.01'],
  },
};

function pick(paths) {
  return paths[Math.floor(Math.random() * paths.length)];
}

export default function () {
  group('browse', function () {
    for (let i = 0; i < 5; i++) {
      const res = http.get(`${BASE_URL}${pick(BROWSE_PATHS)}`, {
        headers: { Host: VHOST },
        tags: { kind: 'browse' },
      });
      const ok = check(res, { 'browse status < 400': (r) => r.status < 400 });
      errorRate.add(!ok, { kind: 'browse' });
      latency.add(res.timings.duration, { kind: 'browse' });
      sleep(0.2 + Math.random() * 0.5);
    }
  });

  group('login', function () {
    const res = http.post(
      `${BASE_URL}/wp-login.php`,
      { log: 'admin', pwd: 'password' },
      { headers: { Host: VHOST, 'Content-Type': 'application/x-www-form-urlencoded' }, tags: { kind: 'login' } },
    );
    // A wrong password still yields a well-formed 200 page; only transport or
    // server errors count as failures.
    const ok = check(res, { 'login status < 400': (r) => r.status < 400 });
    errorRate.add(!ok, { kind: 'login' });
    latency.add(res.timings.duration, { kind: 'login' });
  });
}
