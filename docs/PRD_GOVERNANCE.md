# Governance PRD: Community Ownership & Moderation

**Status:** Planning / Not Started
**Owner:** Platform Team
**Last Updated:** 2025-10-10

## Overview

Community governance defines who can manage communities, how moderation authority is distributed, and how communities can evolve ownership over time. This PRD outlines the authorization model for community management, from the initial simple role-based system to future decentralized governance.

The governance system must balance three competing needs:
1. **Community autonomy** - Communities should self-govern where possible
2. **Instance control** - Hosting instances need moderation/compliance powers
3. **User experience** - Clear, understandable permissions that work for self-hosted and centralized deployments

## Problem Statement

**Current State (2025-10-10):**
- Communities own their own atProto repositories (V2 architecture)
- Instance holds PDS credentials for infrastructure management
- No authorization model exists for who can update/manage communities
- Only implicit "owner" is the instance itself

**Key Issues:**
1. **Self-hosted instances:** Instance operator can't delegate community management to trusted users
2. **Community lifecycle:** No way to transfer ownership or add co-managers
3. **Scaling moderation:** Single-owner model doesn't scale to large communities
4. **User expectations:** Forum users expect moderator teams, not single-admin models

**User Stories:**
- As a **self-hosted instance owner**, I want to create communities and assign moderators so I don't have to manage everything myself
- As a **community creator**, I want to add trusted moderators to help manage the community
- As a **moderator**, I want clear permissions on what I can/cannot do
- As an **instance admin**, I need emergency moderation powers for compliance/safety

## Architecture Evolution

### V1: Role-Based Authorization (Recommended Starting Point)

**Status:** Planned for initial implementation

**Core Concept:**
Three-tier permission model with clear role hierarchy:

**Roles:**
1. **Creator** - Original community founder (DID from `createdBy` field)
   - Full control: update profile, manage moderators, delete community
   - Can transfer creator role to another user
   - Only one creator per community at a time

2. **Moderator** - Trusted community managers
   - Can update community profile (name, description, avatar, banner)
   - Can manage community content (posts, members)
   - Cannot delete community or manage other moderators
   - Multiple moderators allowed per community

3. **Instance Admin** - Infrastructure operator (implicit role)
   - Emergency override for legal/safety compliance
   - Can delist, quarantine, or remove communities
   - Should NOT be used for day-to-day community management
   - Authority derived from instance DID matching `hostedBy`

**Database Schema:**
```
community_moderators
- id (UUID, primary key)
- community_did (references communities.did)
- moderator_did (user DID)
- role (enum: 'creator', 'moderator')
- added_by (DID of user who granted role)
- added_at (timestamp)
- UNIQUE(community_did, moderator_did)
```

**Authorization Checks:**
- **Update community profile:** Creator OR Moderator
- **Add/remove moderators:** Creator only
- **Delete community:** Creator only
- **Transfer creator role:** Creator only
- **Instance moderation:** Instance admin only (emergency use)

**Implementation Approach:**
- Add `community_moderators` table to schema
- Create authorization middleware for XRPC endpoints
- Update service layer to check permissions before operations
- Store moderator list in both AppView DB and optionally in atProto repository

**Benefits:**
- ✅ Familiar to forum users (creator/moderator model is standard)
- ✅ Works for both centralized and self-hosted instances
- ✅ Clear separation of concerns (community vs instance authority)
- ✅ Easy to implement on top of existing V2 architecture
- ✅ Provides foundation for future governance features

**Limitations:**
- ❌ Still centralized (creator has ultimate authority)
- ❌ No democratic decision-making
- ❌ Moderator removal is unilateral (creator decision)
- ❌ No community input on governance changes

---

### V2: Moderator Tiers & Permissions

**Status:** Future enhancement (6-12 months)

**Concept:**
Expand simple creator/moderator model with granular permissions:

**Permission Types:**
- `manage_profile` - Update name, description, images
- `manage_content` - Moderate posts, remove content
- `manage_members` - Ban users, manage reputation
- `manage_moderators` - Add/remove other moderators
- `manage_settings` - Change visibility, federation settings
- `delete_community` - Permanent deletion

**Moderator Tiers:**
- **Full Moderator:** All permissions except `delete_community`
- **Content Moderator:** Only `manage_content` and `manage_members`
- **Settings Moderator:** Only `manage_profile` and `manage_settings`
- **Custom:** Mix and match individual permissions

**Use Cases:**
- Large communities with specialized mod teams
- Trial moderators with limited permissions
- Automated bots with narrow scopes (e.g., spam removal)

**Trade-offs:**
- More flexible but significantly more complex
- Harder to explain to users
- More surface area for authorization bugs

---

### V3: Democratic Governance (Future Vision)

**Status:** Long-term goal (12-24+ months)

**Concept:**
Communities can opt into democratic decision-making for major actions:

**Governance Models:**
1. **Direct Democracy** - All members vote on proposals
2. **Representative** - Elected moderators serve fixed terms
3. **Sortition** - Random selection of moderators from active members (like jury duty)
4. **Hybrid** - Combination of elected + appointed moderators

**Votable Actions:**
- Adding/removing moderators
- Updating community rules/guidelines
- Changing visibility or federation settings
- Migrating to a different instance
- Transferring creator role
- Deleting/archiving the community

**Governance Configuration:**
- Stored in `social.coves.community.profile` under `governance` field
- Defines voting thresholds (e.g., 60% approval, 10% quorum)
- Sets voting windows (e.g., 7-day voting period)
- Specifies time locks (e.g., 3-day delay before execution)

**Implementation Considerations:**
- Requires on-chain or in-repository voting records for auditability
- Needs sybil-resistance (prevent fake accounts from voting)
- May require reputation/stake minimums to vote
- Should support delegation (I assign my vote to someone else)

**atProto Integration:**
- Votes could be stored as records in community repository
- Enables portable governance (votes migrate with community)
- Allows external tools to verify governance legitimacy

**Benefits:**
- ✅ True community ownership
- ✅ Democratic legitimacy for moderation decisions
- ✅ Resistant to moderator abuse/corruption
- ✅ Aligns with decentralization ethos

**Challenges:**
- ❌ Complex to implement correctly
- ❌ Voting participation often low in practice
- ❌ Vulnerable to brigading/vote manipulation
- ❌ Slower decision-making (may be unacceptable for urgent moderation)
- ❌ Legal/compliance issues (who's liable if community votes for illegal content?)

---

### V4: Multi-Tenant Ownership (Future Vision)

**Status:** Long-term goal (24+ months)

**Concept:**
Communities can be co-owned by multiple entities (users, instances, DAOs) with different ownership stakes:

**Ownership Models:**
1. **Shared Custody** - Multiple DIDs hold credentials (multisig)
2. **Smart Contract Ownership** - On-chain DAO controls community
3. **Federated Ownership** - Distributed across multiple instances
4. **Delegated Ownership** - Community owned by a separate legal entity

**Use Cases:**
- Large communities that span multiple instances
- Communities backed by organizations/companies
- Communities that need legal entity ownership
- Cross-platform communities (exists on multiple protocols)

**Technical Challenges:**
- Credential management with multiple owners (who holds PDS password?)
- Consensus on conflicting actions (one owner wants to delete, one doesn't)
- Migration complexity (transferring ownership stakes)
- Legal structure (who's liable, who pays hosting costs?)

---

## Implementation Roadmap

### Phase 1: V1 Role-Based System (Months 0-3)

**Goals:**
- Ship basic creator/moderator authorization
- Enable self-hosted instances to delegate management
- Foundation for all future governance features

**Deliverables:**
- [ ] Database schema: `community_moderators` table
- [ ] Repository layer: CRUD for moderator records
- [ ] Service layer: Authorization checks for all operations
- [ ] XRPC endpoints:
  - [ ] `social.coves.community.addModerator`
  - [ ] `social.coves.community.removeModerator`
  - [ ] `social.coves.community.listModerators`
  - [ ] `social.coves.community.transferOwnership`
- [ ] Middleware: Role-based authorization for existing endpoints
- [ ] Tests: Integration tests for all permission scenarios
- [ ] Documentation: API docs, governance guide for instance admins

**Success Criteria:**
- Community creators can add/remove moderators
- Moderators can update community profile but not delete
- Authorization prevents unauthorized operations
- Works seamlessly for both centralized and self-hosted instances

---

### Phase 2: Moderator Permissions & Tiers (Months 3-6)

**Goals:**
- Add granular permission system
- Support larger communities with specialized mod teams

**Deliverables:**
- [ ] Schema: Add `permissions` JSON column to `community_moderators`
- [ ] Permission framework: Define and validate permission sets
- [ ] XRPC endpoints:
  - [ ] `social.coves.community.updateModeratorPermissions`
  - [ ] `social.coves.community.getModeratorPermissions`
- [ ] UI-friendly permission presets (Full Mod, Content Mod, etc.)
- [ ] Audit logging: Track permission changes and usage

**Success Criteria:**
- Communities can create custom moderator roles
- Permission checks prevent unauthorized operations
- Clear audit trail of who did what with which permissions

---

### Phase 3: Democratic Governance (Months 6-18)

**Goals:**
- Enable opt-in democratic decision-making
- Support voting on moderators and major community changes

**Deliverables:**
- [ ] Governance framework: Define votable actions and thresholds
- [ ] Voting system: Proposal creation, voting, execution
- [ ] Sybil resistance: Require minimum reputation/activity to vote
- [ ] Lexicon: `social.coves.community.proposal` and `social.coves.community.vote`
- [ ] XRPC endpoints:
  - [ ] `social.coves.community.createProposal`
  - [ ] `social.coves.community.vote`
  - [ ] `social.coves.community.executeProposal`
  - [ ] `social.coves.community.listProposals`
- [ ] Time locks and voting windows
- [ ] Delegation system (optional)

**Success Criteria:**
- Communities can opt into democratic governance
- Proposals can be created, voted on, and executed
- Voting records are portable (stored in repository)
- System prevents vote manipulation

---

### Phase 4: Multi-Tenant Ownership (Months 18+)

**Goals:**
- Research and prototype shared ownership models
- Enable communities backed by organizations/DAOs

**Deliverables:**
- [ ] Research: Survey existing DAO/multisig solutions
- [ ] Prototype: Multisig credential management
- [ ] Legal review: Liability and compliance considerations
- [ ] Integration: Bridge to existing DAO platforms (if applicable)

**Success Criteria:**
- Proof of concept for shared ownership
- Clear legal framework for multi-tenant communities
- Migration path from single-owner to multi-owner

---

## Open Questions

### Phase 1 (V1) Questions
1. **Moderator limit:** Should there be a maximum number of moderators per community?
   - **Recommendation:** Start with soft limit (e.g., 25), raise if needed

2. **Moderator-added moderators:** Can moderators add other moderators, or only the creator?
   - **Recommendation:** Creator-only to start (simpler), add in Phase 2 if needed

3. **Moderator storage:** Store moderator list in atProto repository or just AppView DB?
   - **Recommendation:** AppView DB initially (faster), add repository sync in Phase 2 for portability

4. **Creator transfer:** How to prevent accidental ownership transfers?
   - **Recommendation:** Require confirmation from new creator before transfer completes

5. **Inactive creators:** How to handle communities where creator is gone/inactive?
   - **Recommendation:** Instance admin emergency transfer after X months inactivity (define in Phase 2)

### Phase 2 (V2) Questions
1. **Permission inheritance:** Do higher roles automatically include lower role permissions?
   - Research standard forum software patterns

2. **Permission UI:** How to make granular permissions understandable to non-technical users?
   - Consider permission "bundles" or presets

3. **Permission changes:** Can creator retroactively change moderator permissions?
   - Should probably require confirmation/re-acceptance from moderator

### Phase 3 (V3) Questions
1. **Voter eligibility:** What constitutes "membership" for voting purposes?
   - Active posters? Subscribers? Time-based (member for X days)?

2. **Vote privacy:** Should votes be public or private?
   - Public = transparent, but risk of social pressure
   - Private = freedom, but harder to audit

3. **Emergency override:** Can instance still moderate if community votes for illegal content?
   - Yes (instance liability), but how to make this clear and minimize abuse?

4. **Governance defaults:** What happens to communities that don't explicitly configure governance?
   - Fall back to V1 creator/moderator model

### Phase 4 (V4) Questions
1. **Credential custody:** Who physically holds the PDS credentials in multi-tenant scenario?
   - Multisig wallet? Threshold encryption? Trusted third party?

2. **Cost sharing:** How to split hosting costs across multiple owners?
   - Smart contract? Legal entity? Manual coordination?

3. **Conflict resolution:** What happens when co-owners disagree?
   - Voting thresholds? Arbitration? Fork the community?

---

## Success Metrics

### V1 Launch Metrics
- [ ] 90%+ of self-hosted instances create at least one community
- [ ] Average 2+ moderators per active community
- [ ] Zero authorization bypass bugs in production
- [ ] Creator → Moderator permission model understandable to users (< 5% support tickets about roles)

### V2 Adoption Metrics
- [ ] 20%+ of communities use custom permission sets
- [ ] Zero permission escalation vulnerabilities
- [ ] Audit logs successfully resolve 90%+ of disputes

### V3 Governance Metrics
- [ ] 10%+ of communities opt into democratic governance
- [ ] Average voter turnout > 20% for major decisions
- [ ] < 5% of votes successfully manipulated/brigaded
- [ ] Community satisfaction with governance process > 70%

---

## Technical Decisions Log

### 2025-10-11: Moderator Records Storage Location
**Decision:** Store moderator records in community's repository (`at://community_did/social.coves.community.moderator/{tid}`), not user's repository

**Rationale:**
1. **Federation security**: Community's PDS can write/delete records in its own repo without cross-PDS coordination
2. **Attack resistance**: Malicious self-hosted instances cannot forge or retain moderator status after revocation
3. **Single source of truth**: Community's repo is authoritative; no need to check multiple repos + revocation lists
4. **Instant revocation**: Deleting the record immediately removes moderator status across all instances
5. **Simpler implementation**: No invitation flow, no multi-step acceptance, no revocation reconciliation

**Security Analysis:**
- **Option B (user's repo) vulnerability**: Attacker could self-host malicious AppView that ignores revocation signals stored in community's AppView database, presenting their moderator record as "proof" of authority
- **Option A (community's repo) security**: Even malicious instances must query community's PDS for authoritative moderator list; attacker cannot forge records in community's repository

**Alternatives Considered:**
- **User's repo**: Follows atProto pattern for relationships (like `app.bsky.graph.follow`), provides user consent model, but introduces cross-instance write complexity and security vulnerabilities
- **Hybrid (both repos)**: Assignment in community's repo + acceptance in user's repo provides consent without compromising security, but significantly increases complexity

**Trade-offs Accepted:**
- No explicit user consent (moderators are appointed, not invited)
- Users cannot easily query "what do I moderate?" without AppView index
- Doesn't follow standard atProto relationship pattern (but matches service account pattern like feed generators)

**Implementation Notes:**
- Moderator records are source of truth for permissions
- AppView indexes these records from firehose for efficient querying
- User consent can be added later via optional acceptance records without changing security model
- Matches Bluesky's pattern: relationships in user's repo, service configuration in service's repo

---

### 2025-10-10: V1 Role-Based Model Selected
**Decision:** Start with simple creator/moderator two-tier system

**Rationale:**
- Familiar to users (matches existing forum software)
- Simple to implement on top of V2 architecture
- Works for both centralized and self-hosted instances
- Provides clear migration path to democratic governance
- Avoids over-engineering before we understand actual usage patterns

**Alternatives Considered:**
- **atProto delegation:** More protocol-native, but spec is immature
- **Multisig from day one:** Too complex, unclear user demand
- **Single creator only:** Too limited for real-world use

**Trade-offs Accepted:**
- Won't support democratic governance initially
- Creator still has ultimate authority (not truly decentralized)
- Moderator permissions are coarse-grained

---

## Related PRDs

- [PRD_COMMUNITIES.md](PRD_COMMUNITIES.md) - Core community architecture and V2 implementation
- PRD_MODERATION.md (TODO) - Content moderation, reporting, labeling
- PRD_FEDERATION.md (TODO) - Cross-instance community discovery and moderation

---

## References

- atProto Authorization Spec: https://atproto.com/specs/xrpc#authentication
- Bluesky Moderation System: https://docs.bsky.app/docs/advanced-guides/moderation
- Reddit Moderator System: https://mods.reddithelp.com/hc/en-us/articles/360009381491
- Discord Permission System: https://discord.com/developers/docs/topics/permissions
- DAO Governance Patterns: https://ethereum.org/en/dao/
