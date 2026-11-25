import type { DescMessage, DescMethodUnary, MessageInitShape, MessageShape } from "@bufbuild/protobuf";
import { createQueryOptions } from "@connectrpc/connect-query";
import { useQuery, type QueryClient } from "@tanstack/react-query";
import { createContext, useContext } from "react";
import type { User } from "../gen/platform/v1/identity_pb";
import { platform } from "./platform";

// 按生成描述中的方法名精确失效，保留不相关作用域请求与表单。
export function invalidateMethods(client: QueryClient, names: string[]) {
  return client.invalidateQueries({ predicate: (query) => {
    if (query.queryKey[0] !== "scope") return false;
    const descriptor = query.queryKey.at(-1);
    return !!descriptor && typeof descriptor === "object" && "methodName" in descriptor && names.includes(String(descriptor.methodName));
  } });
}

export const SessionContext = createContext<User | undefined>(undefined);
export const ScopeContext = createContext("home");
export function useUser() {
  const user = useContext(SessionContext);
  if (!user) throw new Error("需要登录后访问");
  return user;
}
export function useRPC<I extends DescMessage, O extends DescMessage>(method: DescMethodUnary<I, O>, input: MessageInitShape<I>, options?: { enabled?: boolean; refetchInterval?: number | false }) {
  const user = useUser();
  const query = createQueryOptions(method, input, { transport: platform.transport });
  const scope = useContext(ScopeContext);
  return useQuery<MessageShape<O>>({
    queryKey: ["scope", user.id, user.authVersion.toString(), scope, ...query.queryKey],
    queryFn: (context) => query.queryFn({ ...context, queryKey: query.queryKey }),
    structuralSharing: query.structuralSharing,
    enabled: options?.enabled,
    refetchInterval: options?.refetchInterval,
    retry: false,
  });
}

export function useExternalLogin(enabled = true) {
  return useQuery({
    queryKey: ["external-login-enabled"],
    queryFn: ({ signal }) => platform.externalLoginEnabled(signal),
    staleTime: 60_000,
    enabled,
    retry: false,
  });
}
