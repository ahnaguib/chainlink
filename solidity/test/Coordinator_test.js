import {
  abiEncode,
  assertActionThrows,
  bigNum,
  consumer,
  checkPublicABI,
  checkServiceAgreementPresent,
  checkServiceAgreementAbsent,
  deploy,
  executeServiceAgreementBytes,
  functionSelector,
  getEvents,
  getLatestEvent,
  initiateServiceAgreement,
  initiateServiceAgreementCall,
  newAddress,
  newHash,
  newServiceAgreement,
  oracleNode,
  pad0xHexTo256Bit,
  padNumTo256Bit,
  personalSign,
  recoverPersonalSignature,
  requestDataBytes,
  requestDataFrom,
  runRequestId,
  sixMonthsFromNow,
  stranger,
  strip0x,
  toHex,
  toWei
} from './support/helpers'

contract('Coordinator', () => {
  const sourcePath = 'Coordinator.sol'
  let coordinator, link

  beforeEach(async () => {
    link = await deploy('link_token/contracts/LinkToken.sol')
    coordinator = await deploy(sourcePath, link.address)
  })

  it('has a limited public interface', () => {
    checkPublicABI(artifacts.require(sourcePath), [
      'getPackedArguments',
      'getId',
      'requestData',
      'fulfillData',
      'getId',
      'initiateServiceAgreement',
      'onTokenTransfer',
      'serviceAgreements',
      'cancel'
    ])
  })

  const agreedPayment = 1
  const agreedExpiration = 2
  const endAt = sixMonthsFromNow()
  const agreedOracles = [
    '0x70AEc4B9CFFA7b55C0711b82DD719049d615E21d',
    '0xd26114cd6EE289AccF82350c8d8487fedB8A0C07'
  ]
  const requestDigest = '0x85820c5ec619a1f517ee6cfeff545ec0ca1a90206e1a38c47f016d4137e801dd'

  const args = [ agreedPayment, agreedExpiration, endAt, agreedOracles, requestDigest ]
  const expectedBinaryArgs = [
    '0x',
    ...[agreedPayment, agreedExpiration, endAt].map(padNumTo256Bit),
    ...agreedOracles.map(pad0xHexTo256Bit),
    strip0x(requestDigest)
  ].join('').toLowerCase()

  describe('#getPackedArguments', () => {
    it('returns the following value, given these arguments', async () => {
      const result = await coordinator.getPackedArguments.call(...args)

      assert.equal(result, expectedBinaryArgs)
    })
  })

  describe('#getId', () => {
    it('matches the ID generated by the oracle off-chain', async () => {
      const expectedBinaryArgsSha3 = web3.sha3(expectedBinaryArgs, { encoding: 'hex' })
      const result = await coordinator.getId.call(...args)

      assert.equal(result, expectedBinaryArgsSha3)
    })
  })

  describe('#initiateServiceAgreement', () => {
    let agreement
    before(async () => {
      agreement = newServiceAgreement({oracles: [oracleNode]})
    })

    context('with valid oracle signatures', () => {
      it('saves a service agreement struct from the parameters', async () => {
        await initiateServiceAgreement(coordinator, agreement)
        await checkServiceAgreementPresent(coordinator, agreement)
      })

      it('returns the SAID', async () => {
        const sAID = await initiateServiceAgreementCall(coordinator, agreement)
        assert.equal(sAID, agreement.id)
      })

      it('logs an event', async () => {
        await initiateServiceAgreement(coordinator, agreement)
        const event = await getLatestEvent(coordinator)
        assert.equal(agreement.id, event.args.said)
      })
    })

    context('with an invalid oracle signatures', () => {
      let badOracleSignature, badRequestDigestAddr
      before(async () => {
        badOracleSignature = personalSign(newAddress(stranger), agreement.id)
        badRequestDigestAddr = recoverPersonalSignature(agreement.id,
                                                        badOracleSignature)
        assert.notEqual(toHex(newAddress(oracleNode)),
                        toHex(badRequestDigestAddr))
      })

      it('saves no service agreement struct, if signatures invalid', async () => {
        await assertActionThrows(
          async () => await initiateServiceAgreement(coordinator,
            Object.assign(agreement, { oracleSignature: badOracleSignature })))
        await checkServiceAgreementAbsent(coordinator, agreement.id)
      })
    })

    context('Validation of service agreement deadlines', () => {
      it('Rejects a service agreement with an endAt date in the past', async () => {
        await assertActionThrows(
          async () => await initiateServiceAgreement(
            coordinator,
            Object.assign(agreement, { endAt: 1 })))
        await checkServiceAgreementAbsent(coordinator, agreement.id)
      })
    })
  })

  describe('#requestData', () => {
    const fHash = functionSelector('requestedBytes32(bytes32,bytes32)')
    const to = '0x80e29acb842498fe6591f020bd82766dce619d43'
    let agreement
    before(() => { agreement = newServiceAgreement({oracles: [oracleNode]}) })

    beforeEach(async () => {
      await initiateServiceAgreement(coordinator, agreement)
      await link.transfer(consumer, toWei(1000))
    })

    context('when called through the LINK token with enough payment', () => {      
      let payload, tx
      beforeEach(async () => {
        const payload = executeServiceAgreementBytes(
          agreement.id, to, fHash, '1', '')
        tx = await link.transferAndCall(coordinator.address, agreement.payment,
                                        payload, { from: consumer })
      })

      it('logs an event', async () => {
        const log = tx.receipt.logs[2]
        assert.equal(coordinator.address, log.address)

        // If updating this test, be sure to update services.ServiceAgreementExecutionLogTopic.
        // (Which see for the calculation of this hash.)
        let eventSignature = '0x6d6db1f8fe19d95b1d0fa6a4bce7bb24fbf84597b35a33ff95521fac453c1529'
        assert.equal(eventSignature, log.topics[0])

        assert.equal(agreement.id, log.topics[1])
        assert.equal(consumer, web3.toDecimal(log.topics[2]))
        assert.equal(agreement.payment, web3.toDecimal(log.topics[3]))
      })
    })

    context('when called through the LINK token with not enough payment', () => {
      it('throws an error', async () => {
        const calldata = executeServiceAgreementBytes(agreement.id, to, fHash, '1', '')
        const underPaid = bigNum(agreement.payment).sub(1)

        await assertActionThrows(async () => {
          await link.transferAndCall(coordinator.address, underPaid, calldata, {
            from: consumer
          })
        })
      })
    })

    context('when not called through the LINK token', () => {
      it('reverts', async () => {
        await assertActionThrows(async () => {
          await coordinator.requestData(0, 0, 1, agreement.id, to, fHash, 1, '', { from: consumer })
        })
      })
    })
  })

  describe('#fulfillData', () => {
    const externalId = '17'
    let agreement, mock, requestId
    beforeEach(async () => {
      agreement = newServiceAgreement({oracles: [oracleNode]})
      const tx = await initiateServiceAgreement(coordinator, agreement)
      assert.equal(tx.logs[0].args.said, agreement.id)

      mock = await deploy('examples/GetterSetter.sol')
      const fHash = functionSelector('requestedBytes32(bytes32,bytes32)')

      const tx = await initiateServiceAgreement(coordinator, agreement)
      requestId = runRequestId(tx.receipt.logs[2])
      assert.equal(tx.logs[0].args.said, agreement.id)
    })
    
    context('cooperative consumer', () => {
      beforeEach(async () => {
        mock = await deploy('examples/GetterSetter.sol')
        const fHash = functionSelector('requestedBytes32(bytes32,bytes32)')
    
        const payload = executeServiceAgreementBytes(agreement.id, mock.address, fHash, 1, '')
        const tx = await link.transferAndCall(coordinator.address, agreement.payment, payload)
        requestId = runRequestId(tx.receipt.logs[2])
      })
      context('when called by a non-owner', () => {
        xit('raises an error', async () => {
          await assertActionThrows(async () => {
            await coordinator.fulfillData(requestId, 'Hello World!', { from: stranger })
          })
        })
      })

      context('when called by an owner', () => {
        it.skip('raises an error if the request ID does not exist', async () => {
          await assertActionThrows(async () => {
            await coordinator.fulfillData(
              0xdeadbeef, 'Hello World!', { from: oracleNode })
          })
        })

        it('sets the value on the requested contract', async () => {
          await coordinator.fulfillData(requestId, 'Hello World!', { from: oracleNode })

          const mockRequestId = await mock.requestId.call()
          assert.equal(requestId, mockRequestId)

          const currentValue = await mock.getBytes32.call()
          assert.equal('Hello World!', web3.toUtf8(currentValue))
        })

        it('does not allow a request to be fulfilled twice', async () => {
          await coordinator.fulfillData(requestId, 'First message!', { from: oracleNode })
          await assertActionThrows(async () => {
            await coordinator.fulfillData(requestId, 'Second message!!', { from: oracleNode })
          })
        })
      })
    })

    context('with a malicious requester', () => {
      const paymentAmount = toWei(1)

      beforeEach(async () => {
        mock = await deploy('examples/MaliciousRequester.sol', link.address, coordinator.address)
        await link.transfer(mock.address, paymentAmount)
      })

      xit('cannot cancel before the expiration', async () => {
        await assertActionThrows(async () => {
          await mock.maliciousRequestCancel(agreement.id, 'doesNothing(bytes32,bytes32)')
        })
      })

      it('cannot call functions on the LINK token through callbacks', async () => {
        await assertActionThrows(async () => {
          await mock.request(agreement.id, link.address, 'transfer(address,uint256)')
        })
      })

      context('requester lies about amount of LINK sent', () => {
        it('the oracle uses the amount of LINK actually paid', async () => {
          const req = await mock.maliciousPrice(agreement.id)
          const log = req.receipt.logs[3]

          assert.equal(web3.toWei(1), web3.toDecimal(log.topics[3]))
        })
      })
    })

    context('with a malicious consumer', () => {
      const paymentAmount = toWei(1)

      beforeEach(async () => {
        mock = await deploy('examples/MaliciousConsumer.sol', link.address, coordinator.address)
        await link.transfer(mock.address, paymentAmount)
      })

      context('fails during fulfillment', () => {
        beforeEach(async () => {
          await mock.requestData(agreement.id, 'assertFail(bytes32,bytes32)')
          let events = await getEvents(coordinator)
          requestId = events[0].args.requestId
        })

        // needs coordinator withdrawal functionality to meet parity
        xit('allows the oracle node to receive their payment', async () => {
          await coordinator.fulfillData(requestId, 'hack the planet 101', { from: oracleNode })

          const balance = await link.balanceOf.call(oracleNode)
          assert.isTrue(balance.equals(0))

          await coordinator.withdraw(oracleNode, paymentAmount, { from: oracleNode })
          const newBalance = await link.balanceOf.call(oracleNode)
          assert.isTrue(paymentAmount.equals(newBalance))
        })

        it("can't fulfill the data again", async () => {
          await coordinator.fulfillData(requestId, 'hack the planet 101', { from: oracleNode })
          await assertActionThrows(async () => {
            await coordinator.fulfillData(requestId, 'hack the planet 102', { from: oracleNode })
          })
        })
      })

      context('calls selfdestruct', () => {
        beforeEach(async () => {
          await mock.requestData(agreement.id, 'doesNothing(bytes32,bytes32)')
          let events = await getEvents(coordinator)
          requestId = events[0].args.requestId
          await mock.remove()
        })

        // needs coordinator withdrawal functionality to meet parity
        xit('allows the oracle node to receive their payment', async () => {
          await coordinator.fulfillData(requestId, 'hack the planet 101', { from: oracleNode })

          const balance = await link.balanceOf.call(oracleNode)
          assert.isTrue(balance.equals(0))

          await coordinator.withdraw(oracleNode, paymentAmount, { from: oracleNode })
          const newBalance = await link.balanceOf.call(oracleNode)
          assert.isTrue(paymentAmount.equals(newBalance))
        })
      })

      context('request is canceled during fulfillment', () => {
        beforeEach(async () => {
          await mock.requestData(agreement.id, 'cancelRequestOnFulfill(bytes32,bytes32)')
          let events = await getEvents(coordinator)
          requestId = events[0].args.requestId

          const mockBalance = await link.balanceOf.call(mock.address)
          assert.isTrue(mockBalance.equals(0))
        })

        // needs coordinator withdrawal functionality to meet parity
        xit('allows the oracle node to receive their payment', async () => {
          await coordinator.fulfillData(requestId, 'hack the planet 101', { from: oracleNode })

          const mockBalance = await link.balanceOf.call(mock.address)
          assert.isTrue(mockBalance.equals(0))

          const balance = await link.balanceOf.call(oracleNode)
          assert.isTrue(balance.equals(0))

          await coordinator.withdraw(oracleNode, paymentAmount, { from: oracleNode })
          const newBalance = await link.balanceOf.call(oracleNode)
          assert.isTrue(paymentAmount.equals(newBalance))
        })

        it("can't fulfill the data again", async () => {
          await coordinator.fulfillData(requestId, 'hack the planet 101', { from: oracleNode })
          await assertActionThrows(async () => {
            await coordinator.fulfillData(requestId, 'hack the planet 102', { from: oracleNode })
          })
        })
      })
    })
  })
})
