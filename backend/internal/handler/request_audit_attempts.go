package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func appendOpenAITransportAttempts(
	attempts []service.RequestAuditAttempt,
	metadata []httpattempt.Metadata,
	from int,
	clientModel string,
	postNormalizeModel string,
	fp *service.RequestAuditFingerprintInput,
	inboundProtocol string,
) []service.RequestAuditAttempt {
	if from < 0 {
		from = 0
	}
	if from > len(metadata) {
		from = len(metadata)
	}
	metadata = metadata[from:]
	if len(metadata) == 0 {
		return attempts
	}
	if len(attempts) == 0 {
		attempts = append(attempts,
			service.RequestAuditAttempt{
				Protocol:         inboundProtocol,
				Stage:            service.RequestAuditStageClientEntry,
				ModelFingerprint: fp.DigestModel(clientModel),
			},
			service.RequestAuditAttempt{
				Protocol:         inboundProtocol,
				Stage:            service.RequestAuditStagePostNormalize,
				ModelFingerprint: fp.DigestModel(postNormalizeModel),
			},
		)
	}
	return append(attempts, service.RequestAuditAttemptsFromHTTPMetadata(metadata, fp)...)
}
