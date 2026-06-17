document.addEventListener('DOMContentLoaded', () => {
  let allPackages = [];
  const container = document.getElementById('packages-container');
  const searchInput = document.getElementById('search-input');
  const totalPackagesStat = document.getElementById('stat-total-packages');

  async function fetchPackages() {
    try {
      const res = await fetch('/api/packages');
      if (!res.ok) throw new Error('Registry query failed');
      allPackages = await res.json();
      
      // Sort packages alphabetically
      allPackages.sort((a, b) => a.name.localeCompare(b.name));
      
      updateStats(allPackages);
      renderPackages(allPackages);
    } catch (err) {
      console.error('Failed to load packages:', err);
      container.innerHTML = `
        <div class="empty-state">
          <p style="color:var(--primary); font-size:1.5rem;">✕</p>
          <p>Failed to load packages from registry server.</p>
        </div>
      `;
    }
  }

  function updateStats(packages) {
    totalPackagesStat.textContent = packages.length;
  }

  function formatBytes(bytes) {
    if (bytes === 0) return '0 Bytes';
    const k = 1024;
    const sizes = ['Bytes', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
  }

  function renderPackages(packages) {
    container.innerHTML = '';
    
    if (packages.length === 0) {
      container.innerHTML = `
        <div class="empty-state">
          <p style="font-size:1.5rem;">❖</p>
          <p>No packages found in the registry.</p>
        </div>
      `;
      return;
    }

    packages.forEach(pkg => {
      const card = document.createElement('div');
      card.className = 'package-card glass-card';
      
      const dateStr = new Date(pkg.lastModified).toLocaleDateString(undefined, {
        month: 'short',
        day: 'numeric',
        year: 'numeric'
      });
      
      const sizeStr = formatBytes(pkg.size);
      const installCmd = `serv add ${pkg.name}`;

      card.innerHTML = `
        <div class="pkg-header">
          <div class="pkg-name-wrap">
            <h3>${escapeHtml(pkg.name)}</h3>
            <div class="pkg-meta">
              <span class="pkg-tag">${sizeStr}</span>
              <span class="pkg-tag">Published: ${dateStr}</span>
            </div>
          </div>
          <a class="pkg-action-btn" href="/packages/${pkg.name}.tar.gz" title="Download tarball">
            <svg viewBox="0 0 24 24">
              <path d="M5 20h14v-2H5v2zM19 9h-4V3H9v6H5l7 7 7-7z"/>
            </svg>
          </a>
        </div>
        <div class="cmd-snippet" data-cmd="${installCmd}" title="Click to copy install command">
          <span class="cmd-text">$ ${installCmd}</span>
          <svg class="copy-icon" viewBox="0 0 24 24">
            <path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/>
          </svg>
        </div>
      `;
      
      // Wire up copy click handler
      const snippet = card.querySelector('.cmd-snippet');
      snippet.addEventListener('click', () => {
        navigator.clipboard.writeText(installCmd).then(() => {
          const originalColor = snippet.querySelector('.cmd-text').style.color;
          const originalText = snippet.querySelector('.cmd-text').textContent;
          
          snippet.querySelector('.cmd-text').style.color = '#10b981';
          snippet.querySelector('.cmd-text').textContent = 'Copied to clipboard!';
          
          setTimeout(() => {
            snippet.querySelector('.cmd-text').style.color = originalColor;
            snippet.querySelector('.cmd-text').textContent = originalText;
          }, 1500);
        });
      });
      
      container.appendChild(card);
    });
  }

  function escapeHtml(str) {
    if (!str) return '';
    return str.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#039;");
  }

  // Live local search filtering
  searchInput.addEventListener('input', (e) => {
    const q = e.target.value.toLowerCase().trim();
    if (q === '') {
      renderPackages(allPackages);
      return;
    }
    const filtered = allPackages.filter(pkg => pkg.name.toLowerCase().includes(q));
    renderPackages(filtered);
  });

  // Initial load
  fetchPackages();
});
