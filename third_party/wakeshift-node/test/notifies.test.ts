import { strict as assert } from "node:assert";
import { test } from "node:test";
import { EVENT_TOOL_TEXT, notifies } from "../src/index.js";

const T = "This tool is how {app} notifies you in this conversation{when}. {App} delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it.";
const sentence = (app: string, when = "") => T.replaceAll("{when}", when ? ` when ${when}` : "").replaceAll("{App}", app[0].toUpperCase() + app.slice(1)).replaceAll("{app}", app);

test("notifies: a subscribing tool's description gets the sentences, once, after its own, naming the app and saying when where given", () => {
  assert.equal(EVENT_TOOL_TEXT.notifies, T);
  assert.equal(notifies("Watch a build until it finishes. "), `Watch a build until it finishes. ${sentence("this app")}`);
  assert.equal(
    sentence("Shipyard", "a build finishes"),
    "This tool is how Shipyard notifies you in this conversation when a build finishes. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it.",
  );
  const o = { app: "Shipyard", when: "a build finishes." };
  assert.equal(notifies(notifies("Watch a build.", o), o), `Watch a build. ${sentence("Shipyard", "a build finishes")}`);
  assert.equal(notifies("", { app: " " }), sentence("this app"));
});
