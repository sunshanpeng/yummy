import { createRootRoute, createRoute, createRouter, lazyRouteComponent, Outlet, redirect } from "@tanstack/react-router";
import { ConsoleRoot, AuthenticatedShell } from "./Console";
import { AuthPage } from "./pages/auth";
import { AccountPage } from "./pages/account";
import { ApplicationsPage, ApplicationAccess, ApplicationBasic, ApplicationLayout, WorkspaceHome, WorkspaceLayout, WorkspaceManagement } from "./pages/workspaces";
import { Notice } from "./components/console-kit";
import { errorMessage } from "./lib/platform";

const search = (raw: Record<string, unknown>): { cursor?: string } => ({ cursor: typeof raw.cursor === "string" ? raw.cursor : undefined });
const root = createRootRoute({ component: ConsoleRoot, notFoundComponent: () => <div className="page"><Notice>页面不存在，请从顶部返回工作空间。</Notice></div>, errorComponent: ({ error }) => <div className="page"><Notice tone="error">页面加载失败：{errorMessage(error)}</Notice></div> });
const login = createRoute({ getParentRoute: () => root, path: "/login", validateSearch: (raw: Record<string, unknown>): { redirect?: string; return_request?: string } => ({ redirect: typeof raw.redirect === "string" ? raw.redirect : undefined, return_request: typeof raw.return_request === "string" ? raw.return_request : undefined }), component: () => <AuthPage mode="login" /> });
const invite = createRoute({ getParentRoute: () => root, path: "/accept-invite", component: () => <AuthPage mode="invite" /> });
const reset = createRoute({ getParentRoute: () => root, path: "/reset-password", component: () => <AuthPage mode="reset" /> });
const authenticated = createRoute({ getParentRoute: () => root, id: "authenticated", component: AuthenticatedShell });
const home = createRoute({ getParentRoute: () => authenticated, path: "/", validateSearch: search, component: WorkspaceHome });
const account = createRoute({ getParentRoute: () => authenticated, path: "/account", component: () => <AccountPage /> });
const tokens = createRoute({ getParentRoute: () => authenticated, path: "/account/tokens", component: () => <AccountPage tokens /> });
const connections = createRoute({ getParentRoute: () => authenticated, path: "/account/connections", component: lazyRouteComponent(() => import("./pages/oauth"), "ConnectionsPage") });
const authorize = createRoute({ getParentRoute: () => authenticated, path: "/authorize", validateSearch: (raw: Record<string, unknown>): { request_id?: string } => ({ request_id: typeof raw.request_id === "string" ? raw.request_id : undefined }), component: lazyRouteComponent(() => import("./pages/oauth"), "OAuthConsentPage") });
const global = createRoute({ getParentRoute: () => authenticated, path: "/platform", beforeLoad: () => { throw redirect({ to: "/platform/workspaces" }); } });
const globalWorkspaces = createRoute({ getParentRoute: () => authenticated, path: "/platform/workspaces", validateSearch: search, component: lazyRouteComponent(() => import("./pages/platform"), "PlatformWorkspacesRoute") });
const globalUsers = createRoute({ getParentRoute: () => authenticated, path: "/platform/users", component: lazyRouteComponent(() => import("./pages/platform"), "PlatformUsersRoute") });
const globalNodes = createRoute({ getParentRoute: () => authenticated, path: "/platform/nodes", validateSearch: search, component: lazyRouteComponent(() => import("./pages/platform"), "PlatformNodesRoute") });
const globalRegistries = createRoute({ getParentRoute: () => authenticated, path: "/platform/registries", validateSearch: search, component: lazyRouteComponent(() => import("./pages/platform"), "PlatformRegistriesRoute") });
const globalPolicy = createRoute({ getParentRoute: () => authenticated, path: "/platform/policy", component: lazyRouteComponent(() => import("./pages/platform"), "PlatformPolicyRoute") });
const globalAllocations = createRoute({ getParentRoute: () => authenticated, path: "/platform/allocations", component: lazyRouteComponent(() => import("./pages/platform"), "PlatformAllocationsRoute") });
const globalOAuthClients = createRoute({ getParentRoute: () => authenticated, path: "/platform/oauth-clients", validateSearch: search, component: lazyRouteComponent(() => import("./pages/platform"), "PlatformOAuthClientsRoute") });
const workspace = createRoute({ getParentRoute: () => authenticated, path: "/w/$workspaceSlug", component: WorkspaceLayout });
const apps = createRoute({ getParentRoute: () => workspace, path: "apps", validateSearch: search, component: ApplicationsPage });
const app = createRoute({ getParentRoute: () => workspace, path: "apps/$applicationSlug", component: ApplicationLayout });
const overview = createRoute({ getParentRoute: () => app, path: "/", component: lazyRouteComponent(() => import("./pages/overview"), "ApplicationOverview") });
const basic = createRoute({ getParentRoute: () => app, path: "settings/basic", component: ApplicationBasic });
const access = createRoute({ getParentRoute: () => app, path: "settings/access", validateSearch: search, component: ApplicationAccess });
const builds = createRoute({ getParentRoute: () => app, path: "builds", validateSearch: search, component: lazyRouteComponent(() => import("./pages/builds"), "BuildsPage") });
const newBuild = createRoute({ getParentRoute: () => app, path: "builds/new", validateSearch: (raw: Record<string, unknown>): { snapshotId?: string; retryOf?: string } => ({ snapshotId: typeof raw.snapshotId === "string" ? raw.snapshotId : undefined, retryOf: typeof raw.retryOf === "string" ? raw.retryOf : undefined }), component: lazyRouteComponent(() => import("./pages/builds"), "NewBuildPage") });
const buildDetail = createRoute({ getParentRoute: () => app, path: "builds/$buildId", component: lazyRouteComponent(() => import("./pages/builds"), "BuildDetailPage") });
const buildSettings = createRoute({ getParentRoute: () => app, path: "builds/settings", validateSearch: search, component: lazyRouteComponent(() => import("./pages/builds"), "BuildSettingsPage") });
const legacyBuildSettings = createRoute({ getParentRoute: () => app, path: "settings/build", beforeLoad: ({ params }) => { throw redirect({ to: "/w/$workspaceSlug/apps/$applicationSlug/builds/settings", params }); } });
const deployments = createRoute({ getParentRoute: () => app, path: "deployments", validateSearch: (raw: Record<string, unknown>): { cursor?: string; artifactId?: string } => ({ ...search(raw), artifactId: typeof raw.artifactId === "string" ? raw.artifactId : undefined }), component: lazyRouteComponent(() => import("./pages/deployments"), "DeploymentsPage") });
const deploymentDetail = createRoute({ getParentRoute: () => app, path: "deployments/$operationId", component: lazyRouteComponent(() => import("./pages/deployments"), "DeploymentDetailPage") });
const source = createRoute({ getParentRoute: () => app, path: "builds/source", component: lazyRouteComponent(() => import("./pages/source"), "SourcePage") });
const legacySource = createRoute({ getParentRoute: () => app, path: "source", beforeLoad: ({ params }) => { throw redirect({ to: "/w/$workspaceSlug/apps/$applicationSlug/builds/source", params }); } });
const runtime = createRoute({ getParentRoute: () => app, path: "deployments/runtime-config", component: lazyRouteComponent(() => import("./pages/runtime"), "RuntimePage") });
const legacyRuntime = createRoute({ getParentRoute: () => app, path: "settings/runtime", beforeLoad: ({ params }) => { throw redirect({ to: "/w/$workspaceSlug/apps/$applicationSlug/deployments/runtime-config", params }); } });
const manage = createRoute({ getParentRoute: () => workspace, path: "manage", component: Outlet });
const manageIndex = createRoute({ getParentRoute: () => manage, path: "/", beforeLoad: ({ params }) => { throw redirect({ to: "/w/$workspaceSlug/manage/settings", params }); } });
const settings = createRoute({ getParentRoute: () => manage, path: "settings", component: () => <WorkspaceManagement tab="settings" /> });
const members = createRoute({ getParentRoute: () => manage, path: "members", validateSearch: search, component: () => <WorkspaceManagement tab="members" /> });
const invitations = createRoute({ getParentRoute: () => manage, path: "invitations", component: () => <WorkspaceManagement tab="invitations" /> });
const routeTree = root.addChildren([login, invite, reset, authenticated.addChildren([home, account, tokens, connections, authorize, global, globalWorkspaces, globalUsers, globalNodes, globalRegistries, globalPolicy, globalAllocations, globalOAuthClients, workspace.addChildren([apps, app.addChildren([overview, basic, access, runtime, legacyRuntime, source, legacySource, builds, newBuild, buildDetail, buildSettings, legacyBuildSettings, deployments, deploymentDetail]), manage.addChildren([manageIndex, settings, members, invitations])])])]);
export const router = createRouter({ routeTree, defaultPreload: "intent", scrollRestoration: true });

declare module "@tanstack/react-router" {
  interface Register { router: typeof router }
}
