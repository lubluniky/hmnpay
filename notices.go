package hmnpay

import _ "embed"

// The binary is distributed without the source tree, so it carries the
// texts that have to travel with it and the server serves each at a stable
// path: the licence (/license.txt), the additional terms under its section 7
// (/additional-terms.txt) and the third-party notices
// (/third-party-notices.txt).

// License is the text of LICENSE: the GNU General Public License, version 3,
// under which hmnpay is distributed (version 3 or, at your option, any later
// version).
//
//go:embed LICENSE
var License string

// AdditionalTerms is the text of ADDITIONAL_TERMS.md: the copyright notice
// and the additional terms the copyright holders add to the licence as its
// section 7 permits (the "Powered by hmnpay" attribution on payment pages,
// and the names and marks).
//
//go:embed ADDITIONAL_TERMS.md
var AdditionalTerms string

// ThirdPartyNotices is the text of THIRD_PARTY_NOTICES.md: what a build of
// hmnpay carries that is somebody else's work — fonts, vendored code, the
// Go modules compiled into the binary and the JavaScript packages bundled
// into the web interface — with their licences.
//
//go:embed THIRD_PARTY_NOTICES.md
var ThirdPartyNotices string
