import ts from "typescript";

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import { runInNewContext } from "node:vm";

function loadModule(url, globals) {
  const exports = {};
  const { outputText } = ts.transpileModule(readFileSync(url, "utf8"), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, esModuleInterop: true },
  });
  runInNewContext(outputText, { exports, require: createRequire(url), ...globals });
  return exports;
}

const config = loadModule(new URL("../config/app-config.ts", import.meta.url), {});

test("new and legacy JWT accounts display CTEST without changing stored credentials", () => {
  let storedToken;
  const storage = new Map();
  const browser = { document: { cookie: "" } };
  const { auth } = loadModule(new URL("./auth.ts", import.meta.url), {
    require: (name) => {
      assert.equal(name, "@/config/app-config");
      return config;
    },
    window: browser,
    document: browser.document,
    localStorage: {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, value),
      removeItem: (key) => storage.delete(key),
    },
    atob,
    process: { env: {} },
  });

  for (const subject of ["CTEST", "ARTEX"]) {
    storedToken = `header.${Buffer.from(JSON.stringify({ sub: subject })).toString("base64url")}.signature`;
    auth.setToken(storedToken);
    const user = auth.getCurrentUser();
    assert.equal(user.name, "CTEST");
    assert.equal(user.username, subject);
    assert.equal(auth.getToken(), storedToken);
    assert.equal(storage.get("artex_token"), storedToken);
    assert.match(browser.document.cookie, /^artex_token=/);
  }
  auth.clearToken();
  assert.equal(auth.getCurrentUser(), null);
  assert.equal(auth.getToken(), null);
  assert.match(browser.document.cookie, /max-age=0/);
});

test("invalid login stays on the form while expired protected requests still clear the session", async () => {
  for (const path of ["/auth/login", "/tasks"]) {
    const redirects = [];
    const removed = [];
    const { http } = loadModule(new URL("./api.ts", import.meta.url), {
      require: (name) => {
        if (name === "@/lib/mock/enabled") return { MOCK: false };
        assert.equal(name, "@/lib/mock/handler");
        return {};
      },
      window: {
        location: {
          set href(value) {
            redirects.push(value);
          },
        },
      },
      document: { cookie: "" },
      localStorage: { getItem: () => null, removeItem: (key) => removed.push(key) },
      fetch: async () => new Response(JSON.stringify({ error: "用户名或密码错误" }), { status: 401 }),
    });
    const isLogin = path === "/auth/login";
    await assert.rejects(http(path), isLogin ? /用户名或密码错误/ : /未授权/);
    assert.deepEqual(redirects, isLogin ? [] : ["/login"]);
    assert.deepEqual(removed, isLogin ? [] : ["artex_token"]);
  }
});
