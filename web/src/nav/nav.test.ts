import { describe, it, expect } from "vitest";
import { pathToNav, navToPath, type Nav } from "./nav";

describe("pathToNav", () => {
  it('/ -> templates', () => expect(pathToNav("/")).toEqual({ section: "templates" }));
  it('empty string -> templates (treat as /)', () => expect(pathToNav("")).toEqual({ section: "templates" }));
  it('/templates -> templates', () => expect(pathToNav("/templates")).toEqual({ section: "templates" }));
  it('/templates/ trailing slash -> templates', () => expect(pathToNav("/templates/")).toEqual({ section: "templates" }));
  it('/templates/<appId> -> app', () => expect(pathToNav("/templates/my-app-1")).toEqual({ section: "app", appId: "my-app-1" }));
  it('/my-apps -> my-apps', () => expect(pathToNav("/my-apps")).toEqual({ section: "my-apps" }));
  it('/publish -> publish', () => expect(pathToNav("/publish")).toEqual({ section: "publish" }));
  it('/spawn/<spawnId> -> spawn', () => expect(pathToNav("/spawn/abc123")).toEqual({ section: "spawn", spawnId: "abc123" }));
  it('/settings -> settings', () => expect(pathToNav("/settings")).toEqual({ section: "settings" }));
  it('/profiles -> profiles', () => expect(pathToNav("/profiles")).toEqual({ section: "profiles" }));
  it('unknown path -> templates', () => expect(pathToNav("/not-a-known-route")).toEqual({ section: "templates" }));
  it('another unknown path -> templates', () => expect(pathToNav("/foo/bar/baz")).toEqual({ section: "templates" }));

  it('strips query string before matching', () =>
    expect(pathToNav("/templates?foo=bar")).toEqual({ section: "templates" }));
  it('strips hash before matching', () =>
    expect(pathToNav("/my-apps#section")).toEqual({ section: "my-apps" }));
  it('strips both query and hash', () =>
    expect(pathToNav("/publish?x=1#y")).toEqual({ section: "publish" }));

  it('decodes URL-encoded appId', () =>
    expect(pathToNav("/templates/hello%20world")).toEqual({ section: "app", appId: "hello world" }));
  it('decodes URL-encoded spawnId', () =>
    expect(pathToNav("/spawn/sp%2Fabc")).toEqual({ section: "spawn", spawnId: "sp/abc" }));
});

describe("navToPath", () => {
  it('templates -> /templates', () => expect(navToPath({ section: "templates" })).toBe("/templates"));
  it('app -> /templates/<appId>', () => expect(navToPath({ section: "app", appId: "my-app-1" })).toBe("/templates/my-app-1"));
  it('my-apps -> /my-apps', () => expect(navToPath({ section: "my-apps" })).toBe("/my-apps"));
  it('publish -> /publish', () => expect(navToPath({ section: "publish" })).toBe("/publish"));
  it('spawn -> /spawn/<spawnId>', () => expect(navToPath({ section: "spawn", spawnId: "abc123" })).toBe("/spawn/abc123"));
  it('settings -> /settings', () => expect(navToPath({ section: "settings" })).toBe("/settings"));
  it('profiles -> /profiles', () => expect(navToPath({ section: "profiles" })).toBe("/profiles"));

  it('URL-encodes appId with special chars', () =>
    expect(navToPath({ section: "app", appId: "hello world" })).toBe("/templates/hello%20world"));
  it('URL-encodes spawnId with slash', () =>
    expect(navToPath({ section: "spawn", spawnId: "sp/abc" })).toBe("/spawn/sp%2Fabc"));
});

// Asymmetry is intentional: we verify pathToNav(navToPath(nav)) === nav only.
// The inverse navToPath(pathToNav(path)) is NOT identity (e.g. "/" normalizes to "/templates").
describe("round-trip: pathToNav(navToPath(nav)) === nav", () => {
  const cases: Nav[] = [
    { section: "templates" },
    { section: "app", appId: "my-app-1" },
    { section: "app", appId: "hello world" }, // id needing encoding
    { section: "my-apps" },
    { section: "publish" },
    { section: "spawn", spawnId: "abc123" },
    { section: "spawn", spawnId: "sp/abc" },   // id needing encoding
    { section: "settings" },
    { section: "profiles" },
  ];

  for (const nav of cases) {
    it(`round-trips ${JSON.stringify(nav)}`, () => {
      expect(pathToNav(navToPath(nav))).toEqual(nav);
    });
  }
});
