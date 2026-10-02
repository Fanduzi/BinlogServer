// input: locale JSON catalogs next to this file, vue-i18n, and @intlify/message-compiler
// output: non-zero exit when a catalog message fails production compile, or form.flavorHint does not render @@server_uuid
// pos: build-time gate for embedded /ui/ copy; Vite dev only warns on the same @ syntax
// note: if this file changes, update this header and module README.md.

import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { baseCompile } from "@intlify/message-compiler";

const dir = dirname(fileURLToPath(import.meta.url));

function compileErrors(message) {
  const errors = [];
  baseCompile(message, {
    onError(err) {
      errors.push(err.message || String(err));
    },
  });
  return errors;
}

const probe = compileErrors("@@server_uuid");
if (probe.length === 0) {
  console.error("[i18n] checker did not reject raw @@server_uuid");
  process.exit(1);
}

process.env.NODE_ENV = "production";
const { createI18n } = await import("vue-i18n");

function leafPaths(value, prefix, out) {
  if (typeof value === "string") {
    out.push(prefix);
    return;
  }
  if (!value || typeof value !== "object") return;
  for (const [key, child] of Object.entries(value)) {
    leafPaths(child, prefix ? `${prefix}.${key}` : key, out);
  }
}

let failed = 0;
const catalogs = readdirSync(dir)
  .filter((name) => name.endsWith(".json"))
  .sort();
if (catalogs.length === 0) {
  console.error("[i18n] no locale catalogs");
  process.exit(1);
}

for (const file of catalogs) {
  const messages = JSON.parse(readFileSync(join(dir, file), "utf8"));
  const paths = [];
  leafPaths(messages, "", paths);
  for (const path of paths) {
    const text = path.split(".").reduce((acc, key) => acc?.[key], messages);
    const errors = compileErrors(text);
    if (errors.length) {
      failed += 1;
      console.error(`[i18n] ${file} ${path}: ${errors.join("; ")}`);
    }
  }

  const i18n = createI18n({
    legacy: false,
    locale: "en",
    fallbackLocale: false,
    messages: { en: messages },
  });
  try {
    const hint = i18n.global.t("form.flavorHint");
    if (typeof hint !== "string" || !hint.includes("@@server_uuid")) {
      failed += 1;
      console.error(`[i18n] ${file} form.flavorHint rendered ${JSON.stringify(hint)}`);
    }
  } catch (err) {
    failed += 1;
    console.error(`[i18n] ${file} form.flavorHint: ${err && err.message ? err.message : err}`);
  }
}

if (failed) process.exit(1);
console.log(`[i18n] compiled ${catalogs.length} locale catalogs`);
