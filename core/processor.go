package core

import (
	"context"

	"github.com/filecoin-project/go-filecoin/actor"
	"github.com/filecoin-project/go-filecoin/actor/builtin/account"
	"github.com/filecoin-project/go-filecoin/address"
	"github.com/filecoin-project/go-filecoin/state"
	"github.com/filecoin-project/go-filecoin/types"
	"github.com/filecoin-project/go-filecoin/vm"
	"github.com/filecoin-project/go-filecoin/vm/errors"
)

// Processor is the signature of a function used to process blocks.
type Processor func(ctx context.Context, blk *types.Block, st state.Tree, vms vm.StorageMap) ([]*ApplicationResult, error)

// TipSetProcessor is the signature of a function used to process tipsets
type TipSetProcessor func(ctx context.Context, ts TipSet, st state.Tree, vms vm.StorageMap) (*ProcessTipSetResponse, error)

// ProcessBlock is the entrypoint for validating the state transitions
// of the messages in a block. When we receive a new block from the
// network ProcessBlock applies the block's messages to the beginning
// state tree ensuring that all transitions are valid, accumulating
// changes in the state tree, and returning the message receipts.
//
// ProcessBlock returns an error if the block contains a message, the
// application of which would result in an invalid state transition (eg, a
// message to transfer value from an unknown account). ProcessBlock
// can return one of three kinds of errors (see ApplyMessage: fault
// error, permanent error, temporary error). For the purposes of
// block validation the caller probably doesn't care if the error
// was temporary or permanent; either way the block has a bad
// message and should be thrown out. Caller should always differentiate
// a fault error as it signals something Very Bad has happened
// (eg, disk corruption).
//
// To be clear about intent: if ProcessBlock returns an ApplyError
// it is signaling that the message should not have been included
// in the block. If no error is returned this means that the
// message was applied, BUT SUCCESSFUL APPLICATION DOES NOT
// NECESSARILY MEAN THE IMPLIED CALL SUCCEEDED OR SENDER INTENT
// WAS REALIZED. It just means that the transition if any was
// valid. For example, a message that errors out in the VM
// will in many cases be successfully applied even though an
// error was thrown causing any state changes to be rolled back.
// See comments on ApplyMessage for specific intent.
func ProcessBlock(ctx context.Context, blk *types.Block, st state.Tree, vms vm.StorageMap) ([]*ApplicationResult, error) {
	var emptyResults []*ApplicationResult
	bh := types.NewBlockHeight(uint64(blk.Height))
	res, faultErr := ApplyMessages(ctx, blk.Messages, st, vms, bh)
	if faultErr != nil {
		return emptyResults, faultErr
	}
	if len(res.PermanentErrors) > 0 {
		return emptyResults, res.PermanentErrors[0]
	}
	if len(res.TemporaryErrors) > 0 {
		return emptyResults, res.TemporaryErrors[0]
	}
	return res.Results, nil
}

// ApplicationResult contains the result of successfully applying one message.
// A message can return an error and still be applied successfully.
// See ApplyMessage() for details.
type ApplicationResult struct {
	Receipt        *types.MessageReceipt
	ExecutionError error
}

// ProcessTipSetResponse records the results of successfully applied messages,
// and the sets of successful and failed message cids.  Information of successes
// and failues is key for helping match user messages with receipts in the case
// of message conflicts
type ProcessTipSetResponse struct {
	Results   []*ApplicationResult
	Successes types.SortedCidSet
	Failures  types.SortedCidSet
}

// ProcessTipSet computes the state transition specified by the messages in all
// blocks in a TipSet.  It is similar to ProcessBlock with a few key differences.
// Most importantly ProcessTipSet relies on the precondition that each input block
// is valid with respect to the base state st, that is, ProcessBlock is free of
// errors when applied to each block individually over the given state.
// ProcessTipSet only returns errors in the case of faults.  Other errors
// coming from calls to ApplyMessage can be traced to different blocks in the
// TipSet containing conflicting messages and are ignored.  Blocks are applied
// in the sorted order of their tickets.
func ProcessTipSet(ctx context.Context, ts TipSet, st state.Tree, vms vm.StorageMap) (*ProcessTipSetResponse, error) {
	var res ProcessTipSetResponse
	var emptyRes ProcessTipSetResponse
	h, err := ts.Height()
	if err != nil {
		return &emptyRes, errors.FaultErrorWrap(err, "processing empty tipset")
	}
	bh := types.NewBlockHeight(h)
	msgFilter := make(map[string]struct{})

	tips := ts.ToSlice()
	types.SortBlocks(tips)

	// TODO: this can be made slightly more efficient by reusing the validation
	// transition of the first validated block (currently done in chain_manager fns).
	for _, blk := range tips {
		// filter out duplicates within TipSet
		var msgs []*types.SignedMessage
		for _, msg := range blk.Messages {
			mCid, err := msg.Cid()
			if err != nil {
				return &emptyRes, errors.FaultErrorWrap(err, "error getting message cid")
			}
			if _, ok := msgFilter[mCid.String()]; ok {
				continue
			}
			msgs = append(msgs, msg)
			// filter all messages that we attempted to apply
			// TODO is there ever a reason to try a duplicate failed message again within the same tipset?
			msgFilter[mCid.String()] = struct{}{}
		}
		amRes, err := ApplyMessages(ctx, msgs, st, vms, bh)
		if err != nil {
			return &emptyRes, err
		}
		res.Results = append(res.Results, amRes.Results...)
		for _, msg := range amRes.SuccessfulMessages {
			mCid, err := msg.Cid()
			if err != nil {
				return &emptyRes, errors.FaultErrorWrap(err, "error getting message cid")
			}
			(&res.Successes).Add(mCid)
		}
		for _, msg := range amRes.PermanentFailures {
			mCid, err := msg.Cid()
			if err != nil {
				return &emptyRes, errors.FaultErrorWrap(err, "error getting message cid")
			}
			(&res.Failures).Add(mCid)
		}
		for _, msg := range amRes.TemporaryFailures {
			mCid, err := msg.Cid()
			if err != nil {
				return &emptyRes, errors.FaultErrorWrap(err, "error getting message cid")
			}
			(&res.Failures).Add(mCid)
		}

	}

	return &res, nil
}

// ApplyMessage attempts to apply a message to a state tree. It is the
// sole driver of state tree transitions in the system. Both block
// validation and mining use this function and we should treat any changes
// to it with extreme care.
//
// If ApplyMessage returns no error then the message was successfully applied
// to the state tree: it did not result in any invalid transitions. As you will see
// below, this does not necessarily mean that the message "succeeded" for some
// senses of "succeeded". We choose therefore to say the message was or was not
// successfully applied.
//
// If ApplyMessage returns an error then the message would've resulted in
// an invalid state transition -- it was not successfully applied. When
// ApplyMessage returns an error one of three predicates will be true:
//   - IsFault(err): a system fault occurred (corrupt disk, violated precondition,
//     etc). This is Bad. Caller should stop doing whatever they are doing and get a doctor.
//     No guarantees are made about the state of the state tree.
//   - IsApplyErrorPermanent: the message was not applied and is unlikely
//     to *ever* be successfully applied (equivalently, it is unlikely to
//     ever result in a valid state transition). For example, the message might
//     attempt to transfer negative value. The message should probably be discarded.
//     All state tree mutations will have been reverted.
//   - IsApplyErrorTemporary: the message was not applied though it is
//     *possible* but not certain that the message may become applyable in
//     the future (eg, nonce is too high). The state was reverted.
//
// Please carefully consider the following intent with respect to messages.
// The intentions span the following concerns:
//   - whether the message was successfully applied: if not don't include it
//     in a block. If so inc sender's nonce and include it in a block.
//   - whether the message might be successfully applied at a later time
//     (IsApplyErrorTemporary) vs not (IsApplyErrorPermanent). If the caller
//     is the mining code it could remove permanently unapplyable messages from
//     the message pool but keep temporarily unapplyable messages around to try
//     applying to a future block.
//   - whether to keep or revert state: should we keep or revert state changes
//     caused by the message and its callees? We always revert state changes
//     from unapplyable messages. We might or might not revert changes from
//     applyable messages.
//
// Specific intentions include:
//   - fault errors: immediately return to the caller no matter what
//   - nonce too low: permanently unapplyable (don't include, revert changes, discard)
// TODO: if we have a re-order of the chain the message with nonce too low could
//       become applyable. Except that we already have a message with that nonce.
//       Maybe give this more careful consideration?
//   - nonce too high: temporarily unapplyable (don't include, revert, keep in pool)
//   - sender account exists but insufficient funds: successfully applied
//       (include it in the block but revert its changes). This an explicit choice
//       to make failing transfers not replayable (just like a bank transfer is not
//       replayable).
//   - sender account does not exist: temporarily unapplyable (don't include, revert,
//       keep in pool). There could be an account-creating message forthcoming.
//       (TODO this is only true while we don't have nonce checking; nonce checking
//       will cover this case in the future)
//   - send to self: permanently unapplyable (don't include in a block, revert changes,
//       discard)
//   - transfer negative value: permanently unapplyable (as above)
//   - all other vmerrors: successfully applied! Include in the block and
//       revert changes. Necessarily all vm errors that are not faults are
//       revert errors.
//   - everything else: successfully applied (include, keep changes)
//
// for example squintinig at this perhaps:
//   - ApplyMessage creates a read-through cache of the state tree
//   - it loads the to and from actor into the cache
//   - changes should accumulate in the actor in callees
//   - nothing deeper than this method has direct access to the state tree
//   - no callee should get a different pointer to the to/from actors
//       (we assume the pointer we have accumulates all the changes)
//   - callees must call GetOrCreate on the cache to create a new actor that will be persisted
//   - ApplyMessage and VMContext.Send() are the only things that should call
//     Send() -- all the user-actor logic goes in ApplyMessage and all the
//     actor-actor logic goes in VMContext.Send
func ApplyMessage(ctx context.Context, st state.Tree, store vm.StorageMap, msg *types.Message, bh *types.BlockHeight) (*ApplicationResult, error) {
	cachedStateTree := state.NewCachedStateTree(st)

	r, err := attemptApplyMessage(ctx, cachedStateTree, store, msg, bh)
	if err == nil {
		err = cachedStateTree.Commit(ctx)
		if err != nil {
			return nil, errors.FaultErrorWrap(err, "could not commit state tree")
		}
	} else if errors.IsFault(err) {
		return nil, err
	} else if !errors.ShouldRevert(err) {
		return nil, errors.NewFaultError("someone is a bad programmer: only return revert and fault errors")
	}

	// Reject invalid state transitions.
	var executionError error
	if err == errAccountNotFound || err == errNonceTooHigh {
		return nil, errors.ApplyErrorTemporaryWrapf(err, "apply message failed")
	} else if err == errSelfSend || err == errNonceTooLow || err == errNonAccountActor || err == errors.Errors[errors.ErrCannotTransferNegativeValue] {
		return nil, errors.ApplyErrorPermanentWrapf(err, "apply message failed")
	} else if err != nil { // nolint: megacheck
		// Return the executionError to caller for informational purposes, but otherwise
		// do nothing. All other vm errors are ok: the state was rolled back
		// above but we applied the message successfully. This intentionally
		// includes errInsufficientFunds because we don't want the message
		// to be replayable.
		executionError = err
		log.Warningf("ApplyMessage execution error: %s", executionError)
	}

	// At this point we consider the message successfully applied so inc
	// the nonce.
	fromActor, err := st.GetActor(ctx, msg.From)
	if err != nil {
		return nil, errors.FaultErrorWrap(err, "couldn't load from actor")
	}
	fromActor.IncNonce()
	if err := st.SetActor(ctx, msg.From, fromActor); err != nil {
		return nil, errors.FaultErrorWrap(err, "could not set from actor after inc nonce")
	}

	return &ApplicationResult{Receipt: r, ExecutionError: executionError}, nil
}

var (
	// These errors are only to be used by ApplyMessage; they shouldn't be
	// used in any other context as they are an implementation detail.
	errAccountNotFound = errors.NewRevertError("account not found")
	errNonceTooHigh    = errors.NewRevertError("nonce too high")
	errNonceTooLow     = errors.NewRevertError("nonce too low")
	errNonAccountActor = errors.NewRevertError("message from non-account actor")
	// TODO we'll eventually handle sending to self.
	errSelfSend = errors.NewRevertError("cannot send to self")
)

// CallQueryMethod calls a method on an actor in the given state tree. It does
// not make any changes to the state/blockchain and is useful for interrogating
// actor state. Block height bh is optional; some methods will ignore it.
func CallQueryMethod(ctx context.Context, st state.Tree, vms vm.StorageMap, to address.Address, method string, params []byte, from address.Address, optBh *types.BlockHeight) ([][]byte, uint8, error) {
	// TODO: don't use from?
	toActor, err := st.GetActor(ctx, to)
	if err != nil {
		return nil, 1, errors.ApplyErrorPermanentWrapf(err, "failed to get To actor")
	}

	// not committing or flushing storage structures guarantees changes won't make it to stored state tree or datastore
	cachedSt := state.NewCachedStateTree(st)

	msg := types.NewQueryMessage(from, to, method, params)

	vmCtx := vm.NewVMContext(nil, toActor, msg, cachedSt, vms, optBh)
	ret, retCode, err := vm.Send(ctx, vmCtx)

	return ret, retCode, err
}

// attemptApplyMessage encapsulates the work of trying to apply the message in order
// to make ApplyMessage more readable. The distinction is that attemptApplyMessage
// should deal with trying got apply the message to the state tree whereas
// ApplyMessage should deal with any side effects and how it should be presented
// to the caller. attemptApplyMessage should only be called from ApplyMessage.
func attemptApplyMessage(ctx context.Context, st *state.CachedTree, store vm.StorageMap, msg *types.Message, bh *types.BlockHeight) (*types.MessageReceipt, error) {
	fromActor, err := st.GetActor(ctx, msg.From)
	if state.IsActorNotFoundError(err) {
		return nil, errAccountNotFound
	} else if err != nil {
		return nil, errors.FaultErrorWrapf(err, "failed to get From actor %s", msg.From)
	}

	if msg.From == msg.To {
		// TODO: handle this
		return nil, errSelfSend
	}

	toActor, err := st.GetOrCreateActor(ctx, msg.To, func() (*actor.Actor, error) {
		// Addresses are deterministic so sending a message to a non-existent address must not install an actor,
		// else actors could be installed ahead of address activation. So here we create the empty, upgradable
		// actor to collect any balance that may be transferred.
		return &actor.Actor{}, nil
	})
	if err != nil {
		return nil, errors.FaultErrorWrap(err, "failed to get To actor")
	}

	// processing an exernal message from an empty actor upgrades it to an account actor.
	if fromActor.Code == nil {
		err = account.UpgradeActor(fromActor)
		if err != nil {
			return nil, errors.FaultErrorWrap(err, "failed to upgrade empty actor")
		}
	}

	// if from actor is not an account actor revert message
	if !fromActor.Code.Equals(types.AccountActorCodeCid) {
		return nil, errNonAccountActor
	}

	if msg.Nonce < fromActor.Nonce {
		return nil, errNonceTooLow
	}
	if msg.Nonce > fromActor.Nonce {
		return nil, errNonceTooHigh
	}

	vmCtx := vm.NewVMContext(fromActor, toActor, msg, st, store, bh)
	ret, exitCode, vmErr := vm.Send(ctx, vmCtx)
	if errors.IsFault(vmErr) {
		return nil, vmErr
	}

	receipt := &types.MessageReceipt{
		ExitCode: exitCode,
	}

	// :( - necessary because go slices aren't covariant and we need to convert
	// from [][]byte to []Bytes
	for _, b := range ret {
		receipt.Return = append(receipt.Return, b)
	}

	return receipt, vmErr
}

// ApplyMessagesResponse is the output struct of ApplyMessages.  It exists to
// prevent callers from mistakenly mixing up outputs of the same type.
type ApplyMessagesResponse struct {
	Results            []*ApplicationResult
	PermanentFailures  []*types.SignedMessage
	TemporaryFailures  []*types.SignedMessage
	SuccessfulMessages []*types.SignedMessage

	// Application Errors
	PermanentErrors []error
	TemporaryErrors []error
}

// ApplyMessages applies messages to a state tree.  It returns an
// ApplyMessagesResponse which wraps the results of message application,
// groupings of messages with permanent failures, temporary failures, and
// successes, and the permanent and temporary errors raised during application.
// ApplyMessages will return an error iff a fault message occurs.
func ApplyMessages(ctx context.Context, messages []*types.SignedMessage, st state.Tree, vms vm.StorageMap, bh *types.BlockHeight) (ApplyMessagesResponse, error) {
	var emptyRet ApplyMessagesResponse
	var ret ApplyMessagesResponse

	for _, smsg := range messages {
		// We only want the message, not its signature, validation should have already happened
		msg := &smsg.Message
		r, err := ApplyMessage(ctx, st, vms, msg, bh)
		// If the message should not have been in the block, bail somehow.
		switch {
		case errors.IsFault(err):
			return emptyRet, err
		case errors.IsApplyErrorPermanent(err):
			ret.PermanentFailures = append(ret.PermanentFailures, smsg)
			ret.PermanentErrors = append(ret.PermanentErrors, err)
			continue
		case errors.IsApplyErrorTemporary(err):
			ret.TemporaryFailures = append(ret.TemporaryFailures, smsg)
			ret.TemporaryErrors = append(ret.TemporaryErrors, err)
			continue
		case err != nil:
			panic("someone is a bad programmer: error is neither fault, perm or temp")
		default:
			ret.SuccessfulMessages = append(ret.SuccessfulMessages, smsg)
			ret.Results = append(ret.Results, r)
		}
	}
	return ret, nil
}
