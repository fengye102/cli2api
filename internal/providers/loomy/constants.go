package loomy

import "time"

// CredentialFormat is the canonical storage payload name for this provider.
const CredentialFormat = "loomy-credential-v1"

const (
	// modelBaseURL is the OpenAI-compatible model gateway. TLS 1.3 stalls on
	// this host, so the transport is capped at 1.2 (see doc.go).
	modelBaseURL = "https://loomyad.xunfei.cn/api/v1"

	// accountBaseURL is the 讯飞 account service (login + userinfo).
	accountBaseURL = "https://account.xfinfr.com"

	// accountAppID identifies the Loomy client to the account service.
	accountAppID = "GM3LOOMY"

	// accessKeyID / accessKeySecret sign every account-service request. They
	// are the constants the shipped Electron client carries, not per-user
	// secrets: the account service derives authority from the session, the
	// signature only proves the caller is a Loomy client at all.
	accessKeyID     = "2thryby66wxi53sk"
	accessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"

	// loomyVersion is sent as `loomy-version`; the gateway only checks that
	// the header exists.
	loomyVersion = "0.9.38"

	// Identity values copied from the client. devid is normally minted per
	// account (see mintIdentity); these are the fallbacks used when a
	// credential carries no identity of its own.
	clientDeviceID   = "web"
	clientWebModelID = "Web"
	clientVersion    = "1.0.0"
	clientUA         = "Loomy|Desktop|Electron|macOS"

	// sessionExpireSeconds is what the client asks for: 14 days, no refresh
	// token.
	sessionExpireSeconds = 14 * 24 * 3600
)

// Paths. The first group hangs off modelBaseURL, the second off accountBaseURL.
const (
	chatPath             = "/chat/completions"
	modelsPath           = "/models"
	pointsRecordsPath    = "/points/records"
	onboardingTasksPath  = "/onboarding/tasks"
	onboardingComplete   = "/onboarding/tasks/complete"
	accountPukeyPath     = "/login/account/getPuKey"
	accountPwdLoginPath  = "/login/account/byPwd"
	accountSendMsgPath   = "/login/phone/sendMsgCode"
	accountCheckCodePath = "/login/phone/checkCode"
	accountBaseInfoPath  = "/userinfo/query/baseInfo"
	accountLogoutPath    = "/login/account/logout"
	pointsQueryAllTypes  = "all"
	pointsFirstPage      = "1"
	quotaPointsPageSize  = "1"
	onboardingOKCode     = "000000"
	onboardingAuthCode   = "100002"
	onboardingBadReqCode = "100001"
)

// Timeouts. The reference implementation leaves chat unbounded (the executor
// owns TTFB deadlines) and caps the metadata reads.
const (
	// probeTimeout bounds /models during a liveness probe.
	probeTimeout = 20 * time.Second
	// quotaTimeout bounds /points/records.
	quotaTimeout = 30 * time.Second
	// accountTimeout bounds one account-service call.
	accountTimeout = 30 * time.Second
	// onboardingTimeout bounds one onboarding call.
	onboardingTimeout = 30 * time.Second
	// catalogCacheTTL bounds how long a fetched model catalog is reused.
	catalogCacheTTL = 10 * time.Minute
	// sessionRenewBefore is how long before expiry a session is renewed
	// proactively instead of waiting for a 401 in the middle of a stream.
	sessionRenewBefore = 6 * time.Hour
)

// quotaUnit is the accounting unit shown on the console card.
const quotaUnit = "积分"

// publicModelPrefix keeps Loomy's model ids from colliding with the same
// models served by other channels in this gateway (GLM-5.3-Flash and
// MiniMax-M3 in particular). A request for the bare id keeps routing to the
// channel that already owns it; loomy/… routes here.
const publicModelPrefix = "loomy/"
