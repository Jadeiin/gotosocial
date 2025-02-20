// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package workers

import (
	"context"
	"net/url"

	"codeberg.org/gruf/go-kv"
	"codeberg.org/gruf/go-logger/v2/level"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/gtscontext"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/messages"
	"github.com/superseriousbusiness/gotosocial/internal/processing/account"
	"github.com/superseriousbusiness/gotosocial/internal/state"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// fediAPI wraps processing functions
// specifically for messages originating
// from the federation/ActivityPub API.
type fediAPI struct {
	state      *state.State
	surface    *surface
	federate   *federate
	wipeStatus wipeStatus
	account    *account.Processor
}

func (p *Processor) EnqueueFediAPI(cctx context.Context, msgs ...messages.FromFediAPI) {
	_ = p.workers.Federator.MustEnqueueCtx(cctx, func(wctx context.Context) {
		// Copy caller ctx values to worker's.
		wctx = gtscontext.WithValues(wctx, cctx)

		// Process worker messages.
		for _, msg := range msgs {
			if err := p.ProcessFromFediAPI(wctx, msg); err != nil {
				log.Errorf(wctx, "error processing fedi API message: %v", err)
			}
		}
	})
}

func (p *Processor) ProcessFromFediAPI(ctx context.Context, fMsg messages.FromFediAPI) error {
	// Allocate new log fields slice
	fields := make([]kv.Field, 3, 5)
	fields[0] = kv.Field{"activityType", fMsg.APActivityType}
	fields[1] = kv.Field{"objectType", fMsg.APObjectType}
	fields[2] = kv.Field{"toAccount", fMsg.ReceivingAccount.Username}

	if fMsg.APIri != nil {
		// An IRI was supplied, append to log
		fields = append(fields, kv.Field{
			"iri", fMsg.APIri,
		})
	}

	// Include GTSModel in logs if appropriate.
	if fMsg.GTSModel != nil &&
		log.Level() >= level.DEBUG {
		fields = append(fields, kv.Field{
			"model", fMsg.GTSModel,
		})
	}

	l := log.WithContext(ctx).WithFields(fields...)
	l.Info("processing from fedi API")

	switch fMsg.APActivityType {

	// CREATE SOMETHING
	case ap.ActivityCreate:
		switch fMsg.APObjectType {

		// CREATE NOTE/STATUS
		case ap.ObjectNote:
			return p.fediAPI.CreateStatus(ctx, fMsg)

		// CREATE FOLLOW (request)
		case ap.ActivityFollow:
			return p.fediAPI.CreateFollowReq(ctx, fMsg)

		// CREATE LIKE/FAVE
		case ap.ActivityLike:
			return p.fediAPI.CreateLike(ctx, fMsg)

		// CREATE ANNOUNCE/BOOST
		case ap.ActivityAnnounce:
			return p.fediAPI.CreateAnnounce(ctx, fMsg)

		// CREATE BLOCK
		case ap.ActivityBlock:
			return p.fediAPI.CreateBlock(ctx, fMsg)

		// CREATE FLAG/REPORT
		case ap.ActivityFlag:
			return p.fediAPI.CreateFlag(ctx, fMsg)

		// CREATE QUESTION
		case ap.ActivityQuestion:
			return p.fediAPI.CreatePollVote(ctx, fMsg)
		}

	// UPDATE SOMETHING
	case ap.ActivityUpdate:
		switch fMsg.APObjectType { //nolint:gocritic

		// UPDATE NOTE/STATUS
		case ap.ObjectNote:
			return p.fediAPI.UpdateStatus(ctx, fMsg)

		// UPDATE PROFILE/ACCOUNT
		case ap.ObjectProfile:
			return p.fediAPI.UpdateAccount(ctx, fMsg)
		}

	// DELETE SOMETHING
	case ap.ActivityDelete:
		switch fMsg.APObjectType {

		// DELETE NOTE/STATUS
		case ap.ObjectNote:
			return p.fediAPI.DeleteStatus(ctx, fMsg)

		// DELETE PROFILE/ACCOUNT
		case ap.ObjectProfile:
			return p.fediAPI.DeleteAccount(ctx, fMsg)
		}
	}

	return gtserror.Newf("unhandled: %s %s", fMsg.APActivityType, fMsg.APObjectType)
}

func (p *fediAPI) CreateStatus(ctx context.Context, fMsg messages.FromFediAPI) error {
	var (
		status *gtsmodel.Status
		err    error
	)

	if fMsg.APObjectModel == nil /* i.e. forwarded */ {
		// Model was not set, deref with IRI (this is a forward).
		// This will also cause the status to be inserted into the db.
		status, err = p.statusFromAPIRI(ctx, fMsg)
	} else {
		// Model is set, ensure we have the most up-to-date model.
		status, err = p.statusFromAPModel(ctx, fMsg)
	}

	if err != nil {
		return gtserror.Newf("error extracting status from federatorMsg: %w", err)
	}

	if status.Account == nil || status.Account.IsRemote() {
		// Either no account attached yet, or a remote account.
		// Both situations we need to parse account URI to fetch it.
		accountURI, err := url.Parse(status.AccountURI)
		if err != nil {
			return gtserror.Newf("error parsing account uri: %w", err)
		}

		// Ensure that account for this status has been deref'd.
		status.Account, _, err = p.federate.GetAccountByURI(
			ctx,
			fMsg.ReceivingAccount.Username,
			accountURI,
		)
		if err != nil {
			return gtserror.Newf("error getting account by uri: %w", err)
		}
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status; uncache the
		// prepared version from all timelines. The status dereferencer
		// functions will ensure necessary ancestors exist before this point.
		p.surface.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	if err := p.surface.timelineAndNotifyStatus(ctx, status); err != nil {
		log.Errorf(ctx, "error timelining and notifying status: %v", err)
	}

	return nil
}

func (p *fediAPI) CreatePollVote(ctx context.Context, fMsg messages.FromFediAPI) error {
	// Cast poll vote type from the worker message.
	vote, ok := fMsg.GTSModel.(*gtsmodel.PollVote)
	if !ok {
		return gtserror.Newf("cannot cast %T -> *gtsmodel.PollVote", fMsg.GTSModel)
	}

	// Insert the new poll vote in the database.
	if err := p.state.DB.PutPollVote(ctx, vote); err != nil {
		return gtserror.Newf("error inserting poll vote in db: %w", err)
	}

	// Ensure the poll vote is fully populated at this point.
	if err := p.state.DB.PopulatePollVote(ctx, vote); err != nil {
		return gtserror.Newf("error populating poll vote from db: %w", err)
	}

	// Ensure the poll on the vote is fully populated to get origin status.
	if err := p.state.DB.PopulatePoll(ctx, vote.Poll); err != nil {
		return gtserror.Newf("error populating poll from db: %w", err)
	}

	// Get the origin status,
	// (also set the poll on it).
	status := vote.Poll.Status
	status.Poll = vote.Poll

	// Interaction counts changed on the source status, uncache from timelines.
	p.surface.invalidateStatusFromTimelines(ctx, vote.Poll.StatusID)

	if *status.Local {
		// Before federating it, increment the
		// poll vote counts on our local copy.
		status.Poll.IncrementVotes(vote.Choices)

		// These were poll votes in a local status, we need to
		// federate the updated status model with latest vote counts.
		if err := p.federate.UpdateStatus(ctx, status); err != nil {
			log.Errorf(ctx, "error federating status update: %v", err)
		}
	}

	return nil
}

func (p *fediAPI) statusFromAPModel(ctx context.Context, fMsg messages.FromFediAPI) (*gtsmodel.Status, error) {
	// AP statusable representation MUST have been set.
	statusable, ok := fMsg.APObjectModel.(ap.Statusable)
	if !ok {
		return nil, gtserror.Newf("cannot cast %T -> ap.Statusable", fMsg.APObjectModel)
	}

	// Status may have been set (no problem if not).
	status, _ := fMsg.GTSModel.(*gtsmodel.Status)

	if status == nil {
		// No status was set, create a bare-bones
		// model for the deferencer to flesh-out,
		// this indicates it is a new (to us) status.
		status = &gtsmodel.Status{

			// if coming in here status will ALWAYS be remote.
			Local: util.Ptr(false),
			URI:   ap.GetJSONLDId(statusable).String(),
		}
	}

	// Call refresh on status to either update existing
	// model, or parse + insert status from statusable data.
	status, _, err := p.federate.RefreshStatus(
		ctx,
		fMsg.ReceivingAccount.Username,
		status,
		statusable,
		false, // Don't force refresh.
	)
	if err != nil {
		return nil, gtserror.Newf("error refreshing status: %w", err)
	}

	return status, nil
}

func (p *fediAPI) statusFromAPIRI(ctx context.Context, fMsg messages.FromFediAPI) (*gtsmodel.Status, error) {
	// There should be a status IRI pinned to
	// the federatorMsg for us to dereference.
	if fMsg.APIri == nil {
		const text = "neither APObjectModel nor APIri set"
		return nil, gtserror.New(text)
	}

	// Get the status + ensure we have
	// the most up-to-date version.
	status, _, err := p.federate.GetStatusByURI(
		ctx,
		fMsg.ReceivingAccount.Username,
		fMsg.APIri,
	)
	if err != nil {
		return nil, gtserror.Newf("error getting status by uri %s: %w", fMsg.APIri, err)
	}

	return status, nil
}

func (p *fediAPI) CreateFollowReq(ctx context.Context, fMsg messages.FromFediAPI) error {
	followRequest, ok := fMsg.GTSModel.(*gtsmodel.FollowRequest)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.FollowRequest", fMsg.GTSModel)
	}

	if err := p.state.DB.PopulateFollowRequest(ctx, followRequest); err != nil {
		return gtserror.Newf("error populating follow request: %w", err)
	}

	if *followRequest.TargetAccount.Locked {
		// Account on our instance is locked: just notify the follow request.
		if err := p.surface.notifyFollowRequest(ctx, followRequest); err != nil {
			log.Errorf(ctx, "error notifying follow request: %v", err)
		}
		return nil
	}

	// Account on our instance is not locked:
	// Automatically accept the follow request
	// and notify about the new follower.
	follow, err := p.state.DB.AcceptFollowRequest(
		ctx,
		followRequest.AccountID,
		followRequest.TargetAccountID,
	)
	if err != nil {
		return gtserror.Newf("error accepting follow request: %w", err)
	}

	if err := p.federate.AcceptFollow(ctx, follow); err != nil {
		log.Errorf(ctx, "error federating follow request accept: %v", err)
	}

	if err := p.surface.notifyFollow(ctx, follow); err != nil {
		log.Errorf(ctx, "error notifying follow: %v", err)
	}

	return nil
}

func (p *fediAPI) CreateLike(ctx context.Context, fMsg messages.FromFediAPI) error {
	fave, ok := fMsg.GTSModel.(*gtsmodel.StatusFave)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.StatusFave", fMsg.GTSModel)
	}

	// Ensure fave populated.
	if err := p.state.DB.PopulateStatusFave(ctx, fave); err != nil {
		return gtserror.Newf("error populating status fave: %w", err)
	}

	if err := p.surface.notifyFave(ctx, fave); err != nil {
		log.Errorf(ctx, "error notifying fave: %v", err)
	}

	// Interaction counts changed on the faved status;
	// uncache the prepared version from all timelines.
	p.surface.invalidateStatusFromTimelines(ctx, fave.StatusID)

	return nil
}

func (p *fediAPI) CreateAnnounce(ctx context.Context, fMsg messages.FromFediAPI) error {
	status, ok := fMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Status", fMsg.GTSModel)
	}

	// Dereference status that this boosts, note
	// that this will handle dereferencing the status
	// ancestors / descendants where appropriate.
	if err := p.federate.DereferenceAnnounce(
		ctx,
		status,
		fMsg.ReceivingAccount.Username,
	); err != nil {
		return gtserror.Newf("error dereferencing announce: %w", err)
	}

	// Generate an ID for the boost wrapper status.
	statusID, err := id.NewULIDFromTime(status.CreatedAt)
	if err != nil {
		return gtserror.Newf("error generating id: %w", err)
	}
	status.ID = statusID

	// Store the boost wrapper status.
	if err := p.state.DB.PutStatus(ctx, status); err != nil {
		return gtserror.Newf("db error inserting status: %w", err)
	}

	// Timeline and notify the announce.
	if err := p.surface.timelineAndNotifyStatus(ctx, status); err != nil {
		log.Errorf(ctx, "error timelining and notifying status: %v", err)
	}

	if err := p.surface.notifyAnnounce(ctx, status); err != nil {
		log.Errorf(ctx, "error notifying announce: %v", err)
	}

	// Interaction counts changed on the boosted status;
	// uncache the prepared version from all timelines.
	p.surface.invalidateStatusFromTimelines(ctx, status.ID)

	return nil
}

func (p *fediAPI) CreateBlock(ctx context.Context, fMsg messages.FromFediAPI) error {
	block, ok := fMsg.GTSModel.(*gtsmodel.Block)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Block", fMsg.GTSModel)
	}

	// Remove each account's posts from the other's timelines.
	//
	// First home timelines.
	if err := p.state.Timelines.Home.WipeItemsFromAccountID(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from block -> target's home timeline: %v", err)
	}

	if err := p.state.Timelines.Home.WipeItemsFromAccountID(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from target -> block's home timeline: %v", err)
	}

	// Now list timelines.
	if err := p.state.Timelines.List.WipeItemsFromAccountID(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from block -> target's list timeline(s): %v", err)
	}

	if err := p.state.Timelines.List.WipeItemsFromAccountID(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from target -> block's list timeline(s): %v", err)
	}

	// Remove any follows that existed between blocker + blockee.
	if err := p.state.DB.DeleteFollow(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow from block -> target: %v", err)
	}

	if err := p.state.DB.DeleteFollow(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow from target -> block: %v", err)
	}

	// Remove any follow requests that existed between blocker + blockee.
	if err := p.state.DB.DeleteFollowRequest(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow request from block -> target: %v", err)
	}

	if err := p.state.DB.DeleteFollowRequest(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow request from target -> block: %v", err)
	}

	return nil
}

func (p *fediAPI) CreateFlag(ctx context.Context, fMsg messages.FromFediAPI) error {
	incomingReport, ok := fMsg.GTSModel.(*gtsmodel.Report)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Report", fMsg.GTSModel)
	}

	// TODO: handle additional side effects of flag creation:
	// - notify admins by dm / notification

	if err := p.surface.emailReportOpened(ctx, incomingReport); err != nil {
		log.Errorf(ctx, "error emailing report opened: %v", err)
	}

	return nil
}

func (p *fediAPI) UpdateAccount(ctx context.Context, fMsg messages.FromFediAPI) error {
	// Parse the old/existing account model.
	account, ok := fMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return gtserror.Newf("cannot cast %T -> *gtsmodel.Account", fMsg.GTSModel)
	}

	// Because this was an Update, the new Accountable should be set on the message.
	apubAcc, ok := fMsg.APObjectModel.(ap.Accountable)
	if !ok {
		return gtserror.Newf("cannot cast %T -> ap.Accountable", fMsg.APObjectModel)
	}

	// Fetch up-to-date bio, avatar, header, etc.
	_, _, err := p.federate.RefreshAccount(
		ctx,
		fMsg.ReceivingAccount.Username,
		account,
		apubAcc,
		true, // Force refresh.
	)
	if err != nil {
		log.Errorf(ctx, "error refreshing account: %v", err)
	}

	return nil
}

func (p *fediAPI) UpdateStatus(ctx context.Context, fMsg messages.FromFediAPI) error {
	// Cast the existing Status model attached to msg.
	existing, ok := fMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.Newf("cannot cast %T -> *gtsmodel.Status", fMsg.GTSModel)
	}

	// Cast the updated ActivityPub statusable object .
	apStatus, _ := fMsg.APObjectModel.(ap.Statusable)

	// Fetch up-to-date attach status attachments, etc.
	status, _, err := p.federate.RefreshStatus(
		ctx,
		fMsg.ReceivingAccount.Username,
		existing,
		apStatus,
		true,
	)
	if err != nil {
		log.Errorf(ctx, "error refreshing status: %v", err)
	}

	// Status representation was refetched, uncache from timelines.
	p.surface.invalidateStatusFromTimelines(ctx, status.ID)

	if status.Poll != nil && status.Poll.Closing {

		// If the latest status has a newly closed poll, at least compared
		// to the existing version, then notify poll close to all voters.
		if err := p.surface.notifyPollClose(ctx, status); err != nil {
			log.Errorf(ctx, "error sending poll notification: %v", err)
		}
	}

	return nil
}

func (p *fediAPI) DeleteStatus(ctx context.Context, fMsg messages.FromFediAPI) error {
	// Delete attachments from this status, since this request
	// comes from the federating API, and there's no way the
	// poster can do a delete + redraft for it on our instance.
	const deleteAttachments = true

	status, ok := fMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Status", fMsg.GTSModel)
	}

	if err := p.wipeStatus(ctx, status, deleteAttachments); err != nil {
		log.Errorf(ctx, "error wiping status: %v", err)
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status;
		// uncache the prepared version from all timelines.
		p.surface.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	return nil
}

func (p *fediAPI) DeleteAccount(ctx context.Context, fMsg messages.FromFediAPI) error {
	account, ok := fMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Account", fMsg.GTSModel)
	}

	if err := p.account.Delete(ctx, account, account.ID); err != nil {
		log.Errorf(ctx, "error deleting account: %v", err)
	}

	return nil
}
