import overview from "./overview";
import channels from "./channels";
import accounts from "./accounts";
import resources from "./resources";
import ops from "./ops";
import settings from "./settings";
import audit from "./audit";
import promptAudit from "./promptAudit";
import plugins from "./plugins";
import { requestTraceEn } from "@/features/request-trace/locale";
import { gatewayMockEn } from "@/features/gateway-mock/locale";

export default {
  ...overview,
  ...channels,
  ...accounts,
  ...resources,
  ...ops,
  ...settings,
  ...audit,
  ...promptAudit,
  ...plugins,
  ...requestTraceEn,
  ...gatewayMockEn,
};
