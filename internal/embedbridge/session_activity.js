(() => {
  const parentOrigin = document.currentScript?.dataset.platformOrigin
  if (!parentOrigin || window.parent === window) return
  let nonce = null
  let sequence = 0
  window.addEventListener('message', event => {
    if (event.source !== window.parent || event.origin !== parentOrigin
      || event.data?.type !== 'platform-session:init'
      || typeof event.data.nonce !== 'string' || !/^[a-f0-9]{64}$/.test(event.data.nonce)) return
    nonce = event.data.nonce
    sequence = 0
  })
  for (const input of ['pointerdown', 'pointermove', 'keydown', 'input', 'compositionend', 'touchstart', 'wheel']) {
    window.addEventListener(input, event => {
      if (!nonce || !event.isTrusted || document.visibilityState !== 'visible') return
      sequence += 1
      window.parent.postMessage({ type: 'platform-session:activity', nonce, sequence, input }, parentOrigin)
    }, { capture: true, passive: true })
  }
})()
