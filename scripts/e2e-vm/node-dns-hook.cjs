const dns = require("node:dns");

const vmIP = process.env.ACC_E2E_VM_IP;
const origin = process.env.ACC_WEB_ORIGIN;
if (vmIP && origin) {
  const vmHost = new URL(origin).hostname;
  const lookup = dns.lookup;
  dns.lookup = function lookupE2EHost(hostname, options, callback) {
    if (hostname !== vmHost) {
      return lookup.apply(this, arguments);
    }
    if (typeof options === "function") {
      callback = options;
      options = {};
    } else if (typeof options === "number") {
      options = { family: options };
    }
    if (options?.all) {
      return process.nextTick(callback, null, [{ address: vmIP, family: 4 }]);
    }
    return process.nextTick(callback, null, vmIP, 4);
  };
}
