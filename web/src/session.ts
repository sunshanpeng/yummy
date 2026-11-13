import { createQueryOptions } from "@connectrpc/connect-query";
import { IdentityService } from "./gen/platform/v1/identity_pb";
import { platform } from "./lib/platform";

export const sessionOptions = {
  ...createQueryOptions(IdentityService.method.getSession, {}, { transport: platform.transport }),
  retry: false,
  staleTime: 30_000,
};
