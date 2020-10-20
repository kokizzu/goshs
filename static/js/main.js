$(document).ready(function () {
  $('#tableData').DataTable({
    paging: false,
    language: {
      info: '_TOTAL_ items',
    },
    order: [[2, 'asc']],
    columnDefs: [
      {
        targets: [0, 1, 5],
        orderable: false,
      },
    ],
  });
});

var inputs = document.querySelectorAll('.uploadFile');

Array.prototype.forEach.call(inputs, function (input) {
  var label = input.nextElementSibling,
    labelVal = label.innerHTML;

  input.addEventListener('change', function (e) {
    var fileName = '';
    if (this.files && this.files.length > 1)
      fileName = (this.getAttribute('data-multiple-caption') || '').replace(
        '{count}',
        this.files.length
      );
    else {
      fileName = e.target.value.split('\\').pop();
    }

    if (fileName) label.querySelector('span').innerHTML = fileName;
    else label.innerHTML = labelVal;
  });
});

var checkboxes = document.querySelectorAll('.downloadBulkCheckbox');

Array.prototype.forEach.call(checkboxes, function (cb) {
  cb.addEventListener('change', function () {
    checkedBoxes = document.querySelectorAll('input[type=checkbox]:checked')
      .length;
    if (checkedBoxes >= 1) {
      document.getElementById('downloadBulkButton').style.display = 'block';
    } else {
      document.getElementById('downloadBulkButton').style.display = 'none';
    }
  });
});

function selectAll() {
  Array.prototype.forEach.call(checkboxes, function (cb) {
    cb.checked = true;
  });
  document.getElementById('downloadBulkButton').style.display = 'block';
}

function selectNone() {
  Array.prototype.forEach.call(checkboxes, function (cb) {
    cb.checked = false;
  });
  document.getElementById('downloadBulkButton').style.display = 'none';
}
