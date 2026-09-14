import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  detectEdgeBlock,
  edgeBlockMessage,
  parseJSONResponse,
} from "./edgeBlock.ts";

// 只回显 key 和插值,断言里就能看出选了哪条文案、带了哪些变量。
const translate = (key, vars = {}) =>
  `${key}(${Object.entries(vars)
    .map(([name, value]) => `${name}=${value}`)
    .join(",")})`;

function response(status, headers = {}) {
  const lower = new Map(
    Object.entries(headers).map(([name, value]) => [name.toLowerCase(), value]),
  );
  return { status, headers: { get: (name) => lower.get(name.toLowerCase()) ?? null } };
}

const CLOUDFLARE_PAGE = `<!DOCTYPE html><html><body>
  <h1>Sorry, you have been blocked</h1>
  <p>Cloudflare Ray ID: <strong class="font-semibold">a3af1292ac7c65fb</strong></p>
</body></html>`;

describe("detectEdgeBlock", () => {
  it("reads the Ray ID from cf-ray in preference to the page body", () => {
    const info = detectEdgeBlock(
      response(403, { "content-type": "text/html", "cf-ray": "9f0b1c2d3e4f5061" }),
      CLOUDFLARE_PAGE,
    );
    assert.deepEqual(info, {
      status: 403,
      vendor: "cloudflare",
      rayId: "9f0b1c2d3e4f5061",
    });
  });

  it("falls back to scraping the Ray ID out of the block page", () => {
    const info = detectEdgeBlock(
      response(403, { "content-type": "text/html" }),
      CLOUDFLARE_PAGE,
    );
    assert.equal(info.vendor, "cloudflare");
    assert.equal(info.rayId, "a3af1292ac7c65fb");
  });

  it("keeps the datacenter suffix that Cloudflare appends to the Ray ID", () => {
    const info = detectEdgeBlock(
      response(503, { "content-type": "text/html" }),
      "<html>Cloudflare Ray ID: a3af1292ac7c65fb-MNL</html>",
    );
    assert.equal(info.rayId, "a3af1292ac7c65fb-MNL");
  });

  it("detects HTML with no content-type header", () => {
    const info = detectEdgeBlock({ status: 502 }, "  <html><body>nginx</body></html>");
    assert.deepEqual(info, { status: 502, vendor: "unknown", rayId: "" });
  });

  it("marks a non-Cloudflare block page as an unknown vendor", () => {
    const info = detectEdgeBlock(
      response(403, { "content-type": "text/html", server: "nginx" }),
      "<html><body>Forbidden</body></html>",
    );
    assert.equal(info.vendor, "unknown");
    assert.equal(info.rayId, "");
  });

  it("returns null for real API responses, including JSON that mentions cloudflare", () => {
    assert.equal(
      detectEdgeBlock(response(200, { "content-type": "application/json" }), '{"ok":true}'),
      null,
    );
    assert.equal(
      detectEdgeBlock(
        response(500, { "content-type": "application/json" }),
        '{"error":"cloudflare tunnel down"}',
      ),
      null,
    );
  });
});

describe("edgeBlockMessage", () => {
  it("names Cloudflare literally and passes the Ray ID through", () => {
    assert.equal(
      edgeBlockMessage({ status: 403, vendor: "cloudflare", rayId: "abc" }, translate),
      "common.edgeBlockedWithRay(vendor=Cloudflare,status=403,rayId=abc)",
    );
  });

  it("drops to the Ray-less message and a translated vendor when both are missing", () => {
    assert.equal(
      edgeBlockMessage({ status: 503, vendor: "unknown", rayId: "" }, translate),
      "common.edgeBlocked(vendor=common.edgeBlockVendor(),status=503)",
    );
  });
});

describe("parseJSONResponse", () => {
  const body = (status, text, headers = {}) => ({
    ...response(status, headers),
    text: async () => text,
  });

  it("parses a normal JSON body", async () => {
    assert.deepEqual(
      await parseJSONResponse(body(200, '{"id":7}', { "content-type": "application/json" }), translate),
      { id: 7 },
    );
  });

  it("returns undefined for an empty body", async () => {
    assert.equal(await parseJSONResponse(body(200, "   "), translate), undefined);
  });

  it("reports who blocked the request instead of a JSON.parse offset", async () => {
    await assert.rejects(
      parseJSONResponse(
        body(403, CLOUDFLARE_PAGE, { "content-type": "text/html", "cf-ray": "abc" }),
        translate,
      ),
      /common\.edgeBlockedWithRay\(vendor=Cloudflare,status=403,rayId=abc\)/,
    );
  });

  it("rethrows the original parse error when the body is malformed JSON, not a block page", async () => {
    await assert.rejects(
      parseJSONResponse(body(200, "{oops", { "content-type": "application/json" }), translate),
      (err) => err instanceof SyntaxError,
    );
  });
});
